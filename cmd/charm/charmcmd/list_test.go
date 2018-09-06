package charmcmd_test

// Licensed under the GPLv3, see LICENCE file for details.

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"gopkg.in/juju/charm.v6"

	"github.com/juju/charmstore-client/cmd/charm/charmcmd"
	"github.com/juju/charmstore-client/internal/entitytesting"
)

func TestList(t *testing.T) {
	RunSuite(qt.New(t), &listSuite{})
}

type listSuite struct {
	*charmstoreEnv
}

func (s *listSuite) Init(c *qt.C) {
	fakeHome(c)
	s.charmstoreEnv = initCharmstoreEnv(c)
}

func (s *listSuite) TestNotLoggedIn(c *qt.C) {
	stdout, stderr, exitCode := run(c.Mkdir(), "list")
	c.Assert(stderr, qt.Equals, "")
	c.Assert(exitCode, qt.Equals, 0)
	c.Assert(stdout, qt.Matches, "not logged into "+charmcmd.ServerURL()+"\n")
}

func (s *listSuite) TestInvalidServerURL(c *qt.C) {
	c.Setenv("JUJU_CHARMSTORE", "#%zz")
	stdout, stderr, exitCode := run(c.Mkdir(), "list")
	c.Assert(stdout, qt.Equals, "")
	c.Assert(exitCode, qt.Equals, 1)
	c.Assert(stderr, qt.Equals, "ERROR invalid URL \"#%zz\" for JUJU_CHARMSTORE: parse #%zz: invalid URL escape \"%zz\"\n")
}

func (s *listSuite) TestListUserProvided(c *qt.C) {
	s.discharger.SetDefaultUser("test-user")
	s.uploadCharmDir(c, charm.MustParseURL("~test-user/utopic/wordpress-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	s.uploadCharmDir(c, charm.MustParseURL("~test-user/vivid/alambic-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	s.uploadCharmDir(c, charm.MustParseURL("~test-user/trusty/alambic-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	s.uploadCharmDir(c, charm.MustParseURL("~someoneelse/trusty/alambic-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	dir := c.Mkdir()
	stdout, stderr, code := run(dir, "list", "-u", "test-user")
	c.Assert(stderr, qt.Equals, "")

	c.Assert(stdout, qt.Equals, "cs:~test-user/vivid/alambic-0\ncs:~test-user/trusty/alambic-0\ncs:~test-user/utopic/wordpress-0\n")
	c.Assert(code, qt.Equals, 0)
}

func (s *listSuite) TestListUserProvidedEmpty(c *qt.C) {
	s.discharger.SetDefaultUser("test-user")
	s.uploadCharmDir(c, charm.MustParseURL("~someoneelse/utopic/wordpress-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	dir := c.Mkdir()
	stdout, stderr, code := run(dir, "list", "-u", "test-user")
	c.Assert(stderr, qt.Equals, "")
	c.Assert(stdout, qt.Equals, "No charms found.\n")
	c.Assert(code, qt.Equals, 0)
}

func (s *listSuite) TestListMultipleUsers(c *qt.C) {
	s.discharger.SetDefaultUser("test-user")
	s.uploadCharmDir(c, charm.MustParseURL("~test-user/vivid/alambic-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	s.uploadCharmDir(c, charm.MustParseURL("~someoneelse/utopic/wordpress-0"), -1, entitytesting.Repo.CharmDir("wordpress"))
	dir := c.Mkdir()
	stdout, stderr, code := run(dir, "list", "-u", "test-user,someoneelse")
	c.Assert(stderr, qt.Equals, "")
	c.Assert(stdout, qt.Equals, "cs:~test-user/vivid/alambic-0\ncs:~someoneelse/utopic/wordpress-0\n")
	c.Assert(code, qt.Equals, 0)
}

// TODO frankban: test the case in which the user name must be retrieved
// from the charm store.
