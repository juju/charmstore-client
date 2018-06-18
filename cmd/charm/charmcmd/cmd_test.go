// Copyright 2014 Canonical Ltd.
// Licensed under the GPLv3, see LICENCE file for details.

package charmcmd_test

import (
	"bytes"
	"context"
	"crypto/sha512"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/juju/juju/osenv"
	"github.com/juju/loggo"
	"github.com/juju/testing"
	"github.com/juju/usso"
	gc "gopkg.in/check.v1"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charm.v6"
	"gopkg.in/juju/charmrepo.v4/csclient"
	"gopkg.in/juju/charmrepo.v4/csclient/params"
	"gopkg.in/juju/charmstore.v5"
	"gopkg.in/juju/idmclient.v1/idmtest"
	bakery2u "gopkg.in/macaroon-bakery.v2-unstable/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
	"gopkg.in/mgo.v2"

	"github.com/juju/charmstore-client/cmd/charm/charmcmd"
)

// run runs a charm plugin subcommand with the given arguments,
// its context directory set to dir. It returns the output of the command
// and its exit code.
func run(dir string, args ...string) (stdout, stderr string, exitCode int) {
	return runWithInput(dir, "", args...)
}

// runWithInput runs a charm plugin subcommand with the given arguments,
// its context directory set to dir, and with standard input set to in.
// It returns the output of the command and its exit code.
func runWithInput(dir string, in string, args ...string) (stdout, stderr string, exitCode int) {
	// Remove the warning writer usually registered by cmd.Log.Start, so that
	// it is possible to run multiple commands in the same test.
	// We are not interested in possible errors here.
	defer loggo.RemoveWriter("warning")
	var stdoutBuf, stderrBuf bytes.Buffer
	ctxt := &cmd.Context{
		Dir:    dir,
		Stdin:  strings.NewReader(in),
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
	}
	exitCode = cmd.Main(charmcmd.New(), ctxt, args)
	return stdoutBuf.String(), stderrBuf.String(), exitCode
}

// commonSuite sets up and tears down a charm store
// server suitable for using to test the client commands against.
type commonSuite struct {
	testing.IsolatedMgoSuite
	testing.FakeHomeSuite

	srv     *httptest.Server
	handler charmstore.HTTPCloseHandler

	dockerSrv     *httptest.Server
	dockerRegistry     *httptest.Server
	dockerAuthServer     *httptest.Server
	dockerHandler *dockerHandler
	dockerAuthHandler *dockerAuthHandler
	dockerRegistryHandler *dockerRegistryHandler
	dockerHost    string

	cookieFile   string
	client       *csclient.Client
	serverParams charmstore.ServerParams
	discharger   *idmtest.Server
}

func (s *commonSuite) SetUpSuite(c *gc.C) {
	s.IsolatedMgoSuite.SetUpSuite(c)
	s.FakeHomeSuite.SetUpSuite(c)
}

func (s *commonSuite) TearDownSuite(c *gc.C) {
	s.FakeHomeSuite.TearDownSuite(c)
	s.IsolatedMgoSuite.TearDownSuite(c)
}

func (s *commonSuite) SetUpTest(c *gc.C) {
	s.IsolatedMgoSuite.SetUpTest(c)
	s.FakeHomeSuite.SetUpTest(c)
	s.startServer(c, s.Session)
	s.client = csclient.New(csclient.Params{
		URL:      s.srv.URL,
		User:     s.serverParams.AuthUsername,
		Password: s.serverParams.AuthPassword,
	})
	s.PatchValue(charmcmd.CSClientServerURL, s.srv.URL)
	s.cookieFile = filepath.Join(c.MkDir(), "cookies")
	s.PatchEnvironment("GOCOOKIES", s.cookieFile)
	s.PatchEnvironment("JUJU_LOGGING_CONFIG", "DEBUG")
	osenv.SetJujuXDGDataHome(testing.JujuXDGDataHomePath())
}

func (s *commonSuite) TearDownTest(c *gc.C) {
	osenv.SetJujuXDGDataHome("")
	if s.discharger != nil {
		s.discharger.Close()
	}
	s.srv.Close()
	s.dockerSrv.Close()
	s.handler.Close()
	s.dockerRegistry.Close()
	s.dockerAuthServer.Close()
	s.FakeHomeSuite.TearDownTest(c)
	s.IsolatedMgoSuite.TearDownTest(c)
}

const minUploadPartSize = 100 * 1024

func (s *commonSuite) startServer(c *gc.C, session *mgo.Session) {

	s.dockerHandler = newDockerHandler()
	s.dockerSrv = httptest.NewServer(s.dockerHandler)
	s.dockerAuthHandler = newDockerAuthHandler()
	s.dockerAuthServer = httptest.NewServer(s.dockerAuthHandler)
	s.dockerRegistryHandler = newDockerRegistryHandler(s.dockerAuthServer.URL)
	s.dockerRegistry = httptest.NewTLSServer(s.dockerRegistryHandler)

	dockerURL, err := url.Parse(s.dockerSrv.URL)
	c.Assert(err, gc.Equals, nil)
	s.dockerHost = dockerURL.Host

	s.PatchEnvironment("DOCKER_HOST", s.dockerSrv.URL)

	s.discharger = idmtest.NewServer()
	s.discharger.AddUser("charmstoreuser")
	s.serverParams = charmstore.ServerParams{
		AuthUsername:          "test-user",
		AuthPassword:          "test-password",
		IdentityLocation:      s.discharger.URL.String(),
		AgentKey:              bakery2uKeyPair(s.discharger.UserPublicKey("charmstoreuser")),
		AgentUsername:         "charmstoreuser",
		PublicKeyLocator:      bakeryV2LocatorToV2uLocator{s.discharger},
		MinUploadPartSize:     minUploadPartSize,
		DockerRegistryAddress: s.dockerHost,
	}
	s.handler, err = charmstore.NewServer(session.DB("charmstore"), nil, "", s.serverParams, charmstore.V5)
	c.Assert(err, gc.IsNil)
	s.srv = httptest.NewServer(s.handler)
}

func (s *commonSuite) uploadCharmDir(c *gc.C, id *charm.URL, promulgatedRevision int, ch *charm.CharmDir) {
	var buf bytes.Buffer
	hash := sha512.New384()
	w := io.MultiWriter(hash, &buf)
	err := ch.ArchiveTo(w)
	c.Assert(err, gc.IsNil)
	s.addEntity(c, id, promulgatedRevision, hash.Sum(nil), bytes.NewReader(buf.Bytes()))
	err = s.client.Put("/"+id.Path()+"/meta/perm/read", []string{params.Everyone, id.User})
	c.Assert(err, gc.IsNil)
}

func (s *commonSuite) uploadBundleDir(c *gc.C, id *charm.URL, promulgatedRevision int, b *charm.BundleDir) {
	var buf bytes.Buffer
	hash := sha512.New384()
	w := io.MultiWriter(hash, &buf)
	err := b.ArchiveTo(w)
	c.Assert(err, gc.IsNil)
	s.addEntity(c, id, promulgatedRevision, hash.Sum(nil), bytes.NewReader(buf.Bytes()))
	err = s.client.Put("/"+id.Path()+"/meta/perm/read", []string{params.Everyone, id.User})
	c.Assert(err, gc.IsNil)
}

func (s *commonSuite) uploadResource(c *gc.C, id *charm.URL, name string, content string) {
	_, err := s.client.UploadResource(id, name, "", strings.NewReader(content), int64(len(content)), nil)
	c.Assert(err, gc.IsNil)
}

func (s *commonSuite) addEntity(c *gc.C, id *charm.URL, promulgatedRevision int, hash []byte, body *bytes.Reader) {
	url := fmt.Sprintf("/%s/archive?hash=%x", id.Path(), hash)
	if promulgatedRevision != -1 {
		pid := *id
		pid.User = ""
		pid.Revision = promulgatedRevision
		url += fmt.Sprintf("&promulgated=%s", &pid)
	}
	req, err := http.NewRequest("PUT", "", body)
	c.Assert(err, gc.IsNil)
	req.Header.Set("Content-Type", "application/zip")
	req.ContentLength = int64(body.Len())
	resp, err := s.client.Do(req, url)
	c.Assert(err, gc.IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, gc.Equals, http.StatusOK)
	err = s.client.Put("/"+id.Path()+"/meta/perm/read", []string{params.Everyone})
	c.Assert(err, gc.IsNil)
}

func (s *commonSuite) publish(c *gc.C, id *charm.URL, channels ...params.Channel) {
	path := id.Path()
	err := s.client.Put("/"+path+"/publish", params.PublishRequest{
		Channels: channels,
	})
	c.Assert(err, gc.IsNil)
	err = s.client.Put("/"+path+"/meta/perm/read", []string{
		params.Everyone, id.User,
	})
	c.Assert(err, gc.IsNil)
}

type cmdSuite struct {
	commonSuite
}

var _ = gc.Suite(&cmdSuite{})

func (s *cmdSuite) TestServerURLFromEnvContext(c *gc.C) {
	// We use the info command as a stand-in for
	// all of the commands, because it is testing
	// functionality in newCharmStoreClient,
	// which all commands use to create the charm
	// store client.

	// Point the default server URL to an invalid URL.
	s.PatchValue(charmcmd.CSClientServerURL, "invalid-url")

	// A first call fails.
	_, stderr, code := run(c.MkDir(), "show", "--list")
	c.Assert(stderr, gc.Matches, "ERROR cannot get metadata endpoints: Get invalid-url/v5/meta/: .*\n")
	c.Assert(code, gc.Equals, 1)

	// After setting the JUJU_CHARMSTORE variable, the call succeeds.
	os.Setenv("JUJU_CHARMSTORE", s.srv.URL)
	_, stderr, code = run(c.MkDir(), "show", "--list")
	c.Assert(stderr, gc.Matches, "")
	c.Assert(code, gc.Equals, 0)
}

var translateErrorTests = []struct {
	about       string
	err         error
	expectError string
}{{
	about: "nil",
	err:   nil,
}, {
	about: "unrecognised error",
	err:   errgo.New("test error"),
}, {
	about: "interaction error",
	err: &httpbakery.InteractionError{
		Reason: errgo.New("test error"),
	},
	expectError: "login failed: test error",
}, {
	about: "Ubuntu SSO error",
	err: &httpbakery.InteractionError{
		Reason: &usso.Error{
			Message: "test usso error",
		},
	},
	expectError: "login failed: test usso error",
}, {
	about: "Ubuntu SSO INVALID_DATA error without extra info",
	err: &httpbakery.InteractionError{
		Reason: &usso.Error{
			Code:    "INVALID_DATA",
			Message: "invalid data",
		},
	},
	expectError: "login failed: invalid data",
}, {
	about: "Ubuntu SSO INVALID_DATA with extra info",
	err: &httpbakery.InteractionError{
		Reason: &usso.Error{
			Code: "INVALID_DATA",
			Extra: map[string]interface{}{
				"key": "value",
			},
		},
	},
	expectError: "login failed: key: value",
}, {
	about: "Ubuntu SSO INVALID_DATA with email extra info",
	err: &httpbakery.InteractionError{
		Reason: &usso.Error{
			Code: "INVALID_DATA",
			Extra: map[string]interface{}{
				"email": []interface{}{
					"value",
				},
			},
		},
	},
	expectError: "login failed: username: value",
}}

func (s *cmdSuite) TestTranslateError(c *gc.C) {
	for i, test := range translateErrorTests {
		c.Logf("%d. %s", i, test.about)
		err := charmcmd.TranslateError(test.err)
		if test.expectError == "" {
			c.Assert(err, gc.Equals, test.err)
		} else {
			c.Assert(err, gc.ErrorMatches, test.expectError)
		}
	}
}

type bakeryV2LocatorToV2uLocator struct {
	locator bakery.ThirdPartyLocator
}

// PublicKeyForLocation implements bakery2u.PublicKeyLocator.
func (l bakeryV2LocatorToV2uLocator) PublicKeyForLocation(loc string) (*bakery2u.PublicKey, error) {
	info, err := l.locator.ThirdPartyInfo(context.TODO(), loc)
	if err != nil {
		return nil, err
	}
	return bakery2uKey(&info.PublicKey), nil
}

func bakery2uKey(key *bakery.PublicKey) *bakery2u.PublicKey {
	var key2u bakery2u.PublicKey
	copy(key2u.Key[:], key.Key[:])
	return &key2u
}

func bakery2uKeyPair(key *bakery.KeyPair) *bakery2u.KeyPair {
	var key2u bakery2u.KeyPair
	copy(key2u.Public.Key[:], key.Public.Key[:])
	copy(key2u.Private.Key[:], key.Private.Key[:])
	return &key2u
}
