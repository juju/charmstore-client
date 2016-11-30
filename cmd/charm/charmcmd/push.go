// Copyright 2015 Canonical Ltd.
// Licensed under the GPLv3, see LICENCE file for details.

package charmcmd

import (
	"fmt"
	"io"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juju/cmd"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charm.v6-unstable"
	"gopkg.in/juju/charmrepo.v2-unstable/csclient"
	"gopkg.in/juju/charmrepo.v2-unstable/csclient/params"
	"launchpad.net/gnuflag"
)

type pushCommand struct {
	cmd.CommandBase

	id     *charm.URL
	srcDir string
	auth   authInfo

	// resources is a map of resource name to filename to be uploaded on push.
	resources map[string]string
}

var pushDoc = `
The push command uploads a charm or bundle from the local directory
to the charm store.

The charm or bundle id must not specify a revision: the revision will be
chosen by the charm store to be one more than any existing revision.
If the id is not specified, the current logged-in charm store user name is
used, and the charm or bundle name is taken from the provided directory name.

The pushed charm or bundle is unpublished and therefore usually only available
to a restricted set of users. See the publish command for info on how to make
charms and bundles available to others.

	charm push .
	charm push /path/to/wordpress wordpress
	charm push . cs:~bob/trusty/wordpress

Resources may be uploaded at the same time by specifying the --resource flag.
Following the resource flag should be a name=filepath pair.  This flag may be
repeated more than once to upload more than one resource.

  charm push . --resource website=~/some/file.tgz --resource config=./docs/cfg.xml

See also the attach subcommand, which can be used to push resources
independently of a charm.
`

func (c *pushCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "push",
		Args:    "<directory> [<charm or bundle id>]",
		Purpose: "push a charm or bundle into the charm store",
		Doc:     pushDoc,
	}
}

func (c *pushCommand) SetFlags(f *gnuflag.FlagSet) {
	addAuthFlag(f, &c.auth)
	f.Var(cmd.StringMap{Mapping: &c.resources}, "resource", "")
	f.Var(cmd.StringMap{Mapping: &c.resources}, "r", "resource to be uploaded to the charmstore")
}

func (c *pushCommand) Init(args []string) error {
	if len(args) == 0 {
		return errgo.New("no charm or bundle directory specified")
	}
	if len(args) > 2 {
		return errgo.New("too many arguments")
	}
	c.srcDir = args[0]
	args = args[1:]
	if len(args) == 0 {
		return nil
	}
	id, err := charm.ParseURL(args[0])
	if err != nil {
		return errgo.Notef(err, "invalid charm or bundle id %q", args[0])
	}
	if id.Revision != -1 {
		return errgo.Newf("charm or bundle id %q is not allowed a revision", args[0])
	}
	c.id = id
	return nil
}

func (c *pushCommand) Run(ctxt *cmd.Context) error {
	client, err := newCharmStoreClient(ctxt, c.auth, params.NoChannel)
	if err != nil {
		return errgo.Notef(err, "cannot create the charm store client")
	}
	defer client.jar.Save()

	// Retrieve the source directory where the charm or bundle lives.
	srcDir := ctxt.AbsPath(c.srcDir)
	if _, err := os.Stat(srcDir); err != nil {
		return errgo.Notef(err, "cannot access charm or bundle")
	}

	// Complete the charm or bundle URL by retrieving the source directory
	// name and the current user if necessary.
	if c.id == nil {
		name := filepath.Base(srcDir)
		c.id, err = charm.ParseURL(name)
		if err != nil {
			return errgo.Newf("cannot use %q as charm or bundle name, please specify a name explicitly", name)
		}
	}
	if c.id.User == "" {
		resp, err := client.WhoAmI()
		if err != nil {
			return errgo.Notef(err, "cannot retrieve current username")
		}
		c.id.User = resp.User
	}

	var ch *charm.CharmDir
	var b *charm.BundleDir
	// Find the entity we want to upload. If the series is
	// specified in the id, we know the kind of entity we
	// need to upload; otherwise we fall back to trying
	// both kinds of entity.
	switch {
	case c.id.Series == "bundle":
		b, err = charm.ReadBundleDir(srcDir)
	case c.id.Series != "":
		ch, err = charm.ReadCharmDir(srcDir)
	default:
		ch, err = charm.ReadCharmDir(srcDir)
		if err == nil {
			break
		}
		if b1, err1 := charm.ReadBundleDir(srcDir); err1 == nil {
			b = b1
			err = nil
			c.id.Series = "bundle"
		}
	}
	if ch != nil {
		// Validate resources before pushing the charm.
		if err := validateResources(c.resources, ch.Meta()); err != nil {
			return errgo.Mask(err)
		}
	}
	// Upload the entity if we've found one.
	switch {
	case err != nil:
		return errgo.Mask(err)
	case ch != nil:
		c.id, err = client.UploadCharm(c.id, ch)
	case b != nil:
		if len(c.resources) > 0 {
			return errgo.New("resources not supported on bundles")
		}
		c.id, err = client.UploadBundle(c.id, b)
	default:
		panic("unreachable")
	}
	if err != nil {
		return errgo.Mask(err)
	}
	fmt.Fprintln(ctxt.Stdout, "url:", c.id)
	fmt.Fprintln(ctxt.Stdout, "channel: unpublished")

	// Update the new charm or bundle with VCS extra information.
	if err := updateExtraInfo(c.id, srcDir, client); err != nil {
		return errgo.Notef(err, "cannot add extra information")
	}

	if ch != nil {
		if err := c.pushResources(ctxt, client.Client, ch.Meta(), ctxt.Stdout); err != nil {
			return errgo.Notef(err, "cannot push charm resources")
		}
	}

	return nil
}

func (c *pushCommand) pushResources(ctxt *cmd.Context, client *csclient.Client, meta *charm.Meta, stdout io.Writer) error {
	// Upload resources in alphabetical order so we do things
	// deterministically.
	resourceNames := make([]string, 0, len(c.resources))
	for name := range c.resources {
		resourceNames = append(resourceNames, name)
	}
	sort.Strings(resourceNames)
	for _, name := range resourceNames {
		filename := ctxt.AbsPath(c.resources[name])
		if err := c.uploadResource(client, name, filename, stdout); err != nil {
			return errgo.Mask(err)
		}
	}
	return nil
}

func (c *pushCommand) uploadResource(client *csclient.Client, name, file string, stdout io.Writer) error {
	f, err := os.Open(file)
	if err != nil {
		return errgo.Mask(err)
	}
	defer f.Close()
	rev, err := client.UploadResource(c.id, name, file, f)
	if err != nil {
		return errgo.Mask(err)
	}
	fmt.Fprintf(stdout, "Uploaded %q as %s-%d\n", file, name, rev)
	return nil
}

// validateResources ensures that all the specified resources were defined in
// the corresponding metadata.
func validateResources(resources map[string]string, meta *charm.Meta) error {
	var unknown []string

	for name := range resources {
		if _, ok := meta.Resources[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	switch {
	case len(unknown) > 1:
		sort.Strings(unknown) // Ensure deterministic output.
		return errgo.Newf("unrecognized resources: %s", strings.Join(unknown, ", "))
	case len(unknown) == 1:
		return errgo.Newf("unrecognized resource %q", unknown[0])
	default:
		return nil
	}
}

func updateExtraInfo(id *charm.URL, srcDir string, client *csClient) error {
	info := getExtraInfo(srcDir)
	if info == nil {
		return nil
	}
	return client.PutExtraInfo(id, info)
}

type vcsRevision struct {
	Authors []vcsAuthor `json:"authors"`
	Date    time.Time   `json:"date"`
	Message string      `json:"message,omitempty"`
	Commit  string      `json:"commit,omitempty"`
	Revno   int         `json:"revno,omitempty"`
}

type vcsAuthor struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

const (
	// entrySep holds the string used to separate individual
	// commits in the formatted log output.
	entrySep = "\x1E"
	// fieldSep holds the string used to separate fields
	// in each entry of the formatted log output.
	fieldSep = "\x1F"
)

var gitLogFormat = strings.Join([]string{
	"%H",
	"%an",
	"%ae",
	"%ai",
	"%B",
}, fieldSep) + entrySep

var hgLogFormat = strings.Join([]string{
	"{node|short}",
	"{author|person}",
	"{author|email}",
	"{date|isodatesec}",
	"{desc}",
}, fieldSep) + entrySep + "\n"

type vcsLogParser struct {
	name  string
	parse func(output string) ([]vcsRevision, error)
	args  []string
}

var vcsLogParsers = []vcsLogParser{{
	name:  "git",
	parse: parseLogOutput,
	args:  []string{"log", "-n10", "--pretty=format:" + gitLogFormat},
}, {
	name:  "bzr",
	parse: parseBzrLogOutput,
	args:  []string{"log", "-l10", "--show-ids"},
}, {
	name:  "hg",
	parse: parseLogOutput,
	args:  []string{"log", "-l10", "--template", hgLogFormat},
}}

func getExtraInfo(srcDir string) map[string]interface{} {
	for _, vcs := range vcsLogParsers {
		if _, err := os.Stat(filepath.Join(srcDir, "."+vcs.name)); err != nil {
			continue
		}
		revisions, err := vcs.getRevisions(srcDir)
		if err == nil {
			return map[string]interface{}{
				"vcs-revisions": revisions,
			}
		}
		logger.Errorf("cannot parse %s log: %v", vcs.name, err)
	}
	return nil
}

func (p vcsLogParser) getRevisions(srcDir string) ([]vcsRevision, error) {
	cmd := exec.Command(p.name, p.args...)
	cmd.Dir = srcDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errgo.Notef(err, "%s command failed", p.name)
	}
	revisions, err := p.parse(string(output))
	if err != nil {
		return nil, outputErr(output, err)
	}
	return revisions, nil
}

// parseLogOutput parses the log output from bzr or git.
// Each commit is terminated with entrySep, and within
// a commit, each field is separated with fieldSep.
func parseLogOutput(output string) ([]vcsRevision, error) {
	var entries []vcsRevision
	commits := strings.Split(output, entrySep)
	for _, commit := range commits {
		commit = strings.TrimSpace(commit)
		if string(commit) == "" {
			continue
		}
		// Split on record separator
		fields := strings.Split(commit, fieldSep)
		if len(fields) < 5 {
			return nil, errgo.Newf("unexpected field count in commit log")
		}
		date, err := time.Parse("2006-01-02 15:04:05 -0700", fields[3])
		if err != nil {
			return nil, errgo.Notef(err, "cannot parse commit timestamp")
		}
		entries = append(entries, vcsRevision{
			Authors: []vcsAuthor{{
				Name:  fields[1],
				Email: fields[2],
			}},
			Date:    date,
			Message: fields[4],
			Commit:  fields[0],
		})
	}
	return entries, nil
}

// outputErr returns an error that assembles some command's output and its
// error, if both output and err are set, and returns only err if output is nil.
func outputErr(output []byte, err error) error {
	if len(output) > 0 {
		return fmt.Errorf("%v\n%s", err, output)
	}
	return err
}

// bzrLogDivider holds the text dividing individual bzr log entries.
// TODO this isn't sufficient, because log output can have
// this divider inside messages. Instead, it would probably
// be better to parse on a line-by-line basis.
const bzrLogDivider = "------------------------------------------------------------"

// parseBzrLogOutput reads the raw bytes output from bzr log and returns
// the entries to be added to extra-info.
func parseBzrLogOutput(bzrlog string) ([]vcsRevision, error) {
	logs := strings.Split(bzrlog, bzrLogDivider)
	lastindex := len(logs) - 1
	if logs[lastindex] == "\nUse --include-merged or -n0 to see merged revisions.\n" {
		logs = logs[:lastindex]
	}
	// Skip the empty item before the first divider.
	if len(logs) > 0 && logs[0] == "" {
		logs = logs[1:]
	}
	if len(logs) == 0 {
		return nil, errgo.Newf("no log entries")
	}
	var revisions []vcsRevision
	for _, log := range logs {
		entry, err := parseBzrLogEntry(log)
		if err != nil {
			return nil, errgo.Notef(err, "cannot parse bzr log entry")
		}
		revisions = append(revisions, vcsRevision{
			Date:    entry.Date,
			Message: entry.Message,
			Revno:   entry.Revno,
			Commit:  entry.Commit,
			Authors: parseEmailAddresses(getApparentAuthors(entry)),
		})
	}
	return revisions, nil
}

type bzrLogEntry struct {
	Revno     int
	Commit    string
	Author    string
	Committer string
	Date      time.Time
	Message   string
}

// parseBzrLogEntry parses a single bzr log commit entry.
func parseBzrLogEntry(entryText string) (bzrLogEntry, error) {
	var entry bzrLogEntry
	lines := strings.Split(entryText, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		kvp := strings.SplitN(line, ":", 2)
		if len(kvp) != 2 {
			logger.Errorf("unexpected line in bzr output: %q", string(line))
			continue
		}
		val := strings.TrimLeft(kvp[1], " ")
		switch kvp[0] {
		case "revno":
			val = strings.TrimSuffix(val, " [merge]")
			revno, err := strconv.Atoi(val)
			if err != nil {
				return bzrLogEntry{}, errgo.Newf("invalid revision number %q", val)
			}
			entry.Revno = revno
		case "revision-id":
			entry.Commit = val
		case "author":
			entry.Author = val
		case "committer":
			entry.Committer = val
		case "timestamp":
			t, err := time.Parse("Mon 2006-01-02 15:04:05 Z0700", val)
			if err != nil {
				return bzrLogEntry{}, errgo.Mask(err)
			}
			entry.Date = t
		case "message":
			// TODO this doesn't preserve the message intact. We
			// should strip off the final \n and the leading two
			// space characters from each line.
			entry.Message = strings.Join(lines[i+1:], "\n")
			return entry, nil
		}
	}
	return bzrLogEntry{}, errgo.Newf("no commit message found in bzr log entry")
}

// parseEmailAddresses is not as flexible as
// https://hg.python.org/cpython/file/430aaeaa8087/Lib/email/utils.py#l222
func parseEmailAddresses(emails string) []vcsAuthor {
	addresses, err := mail.ParseAddressList(emails)
	if err != nil {
		// The address list is invalid. Don't abort because of
		// this - just add the email as a name.
		return []vcsAuthor{{
			Name: emails,
		}}
	}
	authors := make([]vcsAuthor, len(addresses))
	for i, address := range addresses {
		authors[i].Name = address.Name
		authors[i].Email = address.Address
	}
	return authors
}

// getApparentAuthors returns author if it is not empty otherwise, returns committer
// This is a bazaar behavior.
// http://bazaar.launchpad.net/~bzr-pqm/bzr/2.6/view/head:/bzrlib/revision.py#L125
func getApparentAuthors(entry bzrLogEntry) string {
	authors := entry.Author
	if authors == "" {
		authors = entry.Committer
	}
	return authors
}
