package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/filecoin-project/go-hamt-ipld"
	blocks "github.com/ipfs/go-block-format"
	bserv "github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	syncds "github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/ipfs/go-merkledag"
	car "github.com/ipld/go-car"
	_ "github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	ucan "github.com/qri-io/ucan"
	didkey "github.com/qri-io/ucan/didkey"
	"github.com/whyrusleeping/bluesky/types"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"
)

var twitterCaps = ucan.NewNestedCapabilities("POST")

const TwitterDid = "did:key:z6Mkmi4eUvWtRAP6PNB7MnGfUFdLkGe255ftW9sGo28uv44g"

type Server struct {
	Blockstore blockstore.Blockstore
	UcanStore  ucan.TokenStore

	ulk       sync.Mutex
	UserRoots map[string]cid.Cid
	UserDids  map[string]*didkey.ID
}

func main() {

	ds := syncds.MutexWrap(datastore.NewMapDatastore())
	bs := blockstore.NewBlockstore(ds)
	s := &Server{
		UserRoots:  make(map[string]cid.Cid),
		UserDids:   make(map[string]*didkey.ID),
		Blockstore: bs,
		UcanStore:  ucan.NewMemTokenStore(),
	}

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.CORS())

	e.POST("/register", s.handleRegister)

	e.POST("/update", s.handleUserUpdate)
	e.GET("/user/:id", s.handleGetUser)
	e.GET("/.well-known/did.json", s.handleGetDid)
	panic(e.Start(":2583"))
}

func (s *Server) ensureGraphWalkability(ctx context.Context, u *types.User, bs blockstore.Blockstore) error {
	if err := s.graphWalkRec(ctx, u.PostsRoot, bs); err != nil {
		return err
	}

	return nil
}

func (s *Server) graphWalkRec(ctx context.Context, c cid.Cid, bs blockstore.Blockstore) error {
	eitherGet := func(cc cid.Cid) (blocks.Block, error) {
		baseHas, err := s.Blockstore.Has(ctx, cc)
		if err != nil {
			return nil, err
		}

		if baseHas {
			// this graph is already in our blockstore
			return nil, nil
		}

		return bs.Get(ctx, cc)
	}

	b, err := eitherGet(c)
	if err != nil {
		return err
	}

	if b == nil {
		return nil
	}

	var links []cid.Cid
	if err := cbg.ScanForLinks(bytes.NewReader(b.RawData()), func(l cid.Cid) {
		links = append(links, l)
	}); err != nil {
		return err
	}

	for _, l := range links {
		if err := s.graphWalkRec(ctx, l, bs); err != nil {
			return err
		}
	}

	return nil
}

// TODO: we probably want this to be a compare-and-swap
func (s *Server) handleUserUpdate(e echo.Context) error {
	ctx := e.Request().Context()

	// check ucan permission
	encoded := getBearer(e.Request())
	p := ucan.NewTokenParser(twitterAC, ucan.StringDIDPubKeyResolver{}, s.UcanStore.(ucan.CIDBytesResolver))
	token, err := p.ParseAndVerify(ctx, encoded)
	if err != nil {
		return err
	}

	if token.Audience.String() != TwitterDid {
		return fmt.Errorf("Ucan not directed to twitter server")
	}

	checkUser := func(user string) bool {
		att := ucan.Attenuation{
			Rsc: newAccountResource("twitter", "dholms"),
			Cap: twitterCaps.Cap("POST"),
		}

		isGood := token.Attenuations.Contains(ucan.Attenuations{att})

		if !isGood {
			return false
		}

		if token.Issuer.String() != s.UserDids[user].String() {
			return false
		}

		return true
	}

	return s.updateUser(ctx, e.Request(), checkUser)
}

func (s *Server) updateUser(ctx context.Context, req *http.Request, checkUser func(user string) bool) error {
	// The body of the request should be a car file containing any *changed* blocks
	cr, err := car.NewCarReader(req.Body)
	if err != nil {
		return err
	}

	roots := cr.Header.Roots

	if len(roots) != 1 {
		// only allow a single dag to be sent for updates
		return fmt.Errorf("cannot have multiple dag roots")
	}

	ds := syncds.MutexWrap(datastore.NewMapDatastore())
	tmpbs := blockstore.NewBlockstore(ds)

	for {
		blk, err := cr.Next()
		if err != nil {
			if !xerrors.Is(err, io.EOF) {
				return err
			}
			break
		}

		if err := tmpbs.Put(ctx, blk); err != nil {
			return err
		}
	}

	rblk, err := tmpbs.Get(ctx, roots[0])
	if err != nil {
		return err
	}

	// TODO: accept signed root & Verify signature
	// var sroot types.SignedRoot
	// if err := sroot.UnmarshalCBOR(bytes.NewReader(rblk.RawData())); err != nil {
	// 	return err
	// }

	// ublk, err := tmpbs.Get(sroot.User)
	// if err != nil {
	// 	return err
	// }

	var user types.User
	if err := user.UnmarshalCBOR(bytes.NewReader(rblk.RawData())); err != nil {
		return err
	}

	if !checkUser(user.Name) {
		return fmt.Errorf("Ucan does not properly permission user")
	}

	fmt.Println("user update: ", user.Name, user.NextPost, user.PostsRoot)

	if err := s.ensureGraphWalkability(ctx, &user, tmpbs); err != nil {
		return err
	}

	if err := Copy(ctx, tmpbs, s.Blockstore); err != nil {
		return err
	}

	if err := s.updateUserRoot(user.DID, roots[0]); err != nil {
		return err
	}

	return nil
}

func (s *Server) updateUserRoot(did string, rcid cid.Cid) error {
	s.ulk.Lock()
	defer s.ulk.Unlock()

	s.UserRoots[did] = rcid
	return nil
}

func (s *Server) getUser(id string) (cid.Cid, error) {
	s.ulk.Lock()
	defer s.ulk.Unlock()

	c, ok := s.UserRoots[id]
	if !ok {
		return cid.Undef, fmt.Errorf("no such user")
	}

	return c, nil
}

func (s *Server) handleGetUser(c echo.Context) error {
	ctx := c.Request().Context()

	ucid, err := s.getUser(c.Param("id"))
	if err != nil {
		return err
	}

	ds := merkledag.NewDAGService(bserv.New(s.Blockstore, nil))
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEOctetStream)
	return car.WriteCar(ctx, ds, []cid.Cid{ucid}, c.Response().Writer)
}

// TODO: this is the register method I wrote for working with the CLI tool, the
// interesting thing here is that it constructs the beginning of the user data
// object on behalf of the user, registers that information locally, and sends
// it all back to the user
// We need to decide if we like this approach, or if we instead want to have
// the user just send us their graph with/after registration.
func (s *Server) handleRegisterUserAlt(c echo.Context) error {
	ctx := c.Request().Context()
	var body userRegisterBody
	if err := c.Bind(&body); err != nil {
		return err
	}

	cst := cbor.NewCborStore(s.Blockstore)

	u := new(types.User)
	//u.DID = body.DID
	u.Name = body.Name

	rcid, err := s.getEmptyPostsRoot(ctx, cst)
	if err != nil {
		return fmt.Errorf("failed to get empty posts root: %w", err)
	}
	u.PostsRoot = rcid

	cc, err := cst.Put(ctx, u)
	if err != nil {
		return fmt.Errorf("failed to write user to blockstore: %w", err)
	}

	s.updateUserRoot(u.DID, cc)

	ds := merkledag.NewDAGService(bserv.New(s.Blockstore, nil))
	if err := car.WriteCar(ctx, ds, []cid.Cid{cc}, c.Response().Writer); err != nil {
		return fmt.Errorf("failed to write car: %w", err)
	}
	return nil
}

type userRegisterBody struct {
	Name string
}

func (s *Server) handleRegister(e echo.Context) error {
	ctx := e.Request().Context()
	encoded := getBearer(e.Request())

	var body userRegisterBody
	if err := e.Bind(&body); err != nil {
		return err
	}

	p := ucan.NewTokenParser(emptyAC, ucan.StringDIDPubKeyResolver{}, s.UcanStore.(ucan.CIDBytesResolver))
	token, err := p.ParseAndVerify(ctx, encoded)
	if err != nil {
		return err
	}

	if token.Audience.String() != TwitterDid {
		return fmt.Errorf("Ucan not directed to twitter server")
	}

	// TODO: this needs a lock
	if s.UserDids[body.Name] != nil {
		return fmt.Errorf("Username already taken")
	}

	s.UserDids[body.Name] = &token.Issuer

	return nil
}

func Copy(ctx context.Context, from, to blockstore.Blockstore) error {
	ch, err := from.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	for k := range ch {
		blk, err := from.Get(ctx, k)
		if err != nil {
			return err
		}

		if err := to.Put(ctx, blk); err != nil {
			return err
		}
	}

	return nil
}

type serverDid struct {
	Id string `json:"id"`
}

func (s *Server) handleGetDid(e echo.Context) error {
	e.JSON(http.StatusOK, serverDid{Id: TwitterDid})
	return nil
}

func getBearer(req *http.Request) string {
	reqToken := req.Header.Get("Authorization")
	splitToken := strings.Split(reqToken, "Bearer ")
	// TODO: check that we didnt get a malformed authorization header, otherwise the next line will panic
	return splitToken[1]
}

func twitterAC(m map[string]interface{}) (ucan.Attenuation, error) {
	var (
		cap string
		rsc ucan.Resource
	)
	for key, vali := range m {
		val, ok := vali.(string)
		if !ok {
			return ucan.Attenuation{}, fmt.Errorf(`expected attenuation value to be a string`)
		}

		if key == ucan.CapKey {
			cap = val
		} else {
			rsc = newAccountResource(key, val)
		}
	}

	return ucan.Attenuation{
		Rsc: rsc,
		Cap: twitterCaps.Cap(cap),
	}, nil
}

func emptyAC(m map[string]interface{}) (ucan.Attenuation, error) {
	return ucan.Attenuation{}, nil
}

type accountRsc struct {
	t string
	v string
}

// NewStringLengthResource is a silly implementation of resource to use while
// I figure out what an OR filter on strings is. Don't use this.
func newAccountResource(typ, val string) ucan.Resource {
	return accountRsc{
		t: typ,
		v: val,
	}
}

func (r accountRsc) Type() string {
	return r.t
}

func (r accountRsc) Value() string {
	return r.v
}

func (r accountRsc) Contains(b ucan.Resource) bool {
	return r.Type() == b.Type() && r.Value() <= b.Value()
}

func (s *Server) getEmptyPostsRoot(ctx context.Context, cst cbor.IpldStore) (cid.Cid, error) {
	n := hamt.NewNode(cst)
	return cst.Put(ctx, n)
}
