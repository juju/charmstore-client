// Copyright 2015 Canonical Ltd.
// Licensed under the GPLv3, see LICENCE file for details.

package charmcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/gnuflag"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charmrepo.v4/csclient/params"
	"net/url"
)

type listCommand struct {
	cmd.CommandBase
	auth authInfo
	out  cmd.Output
	users string
}

var listDoc = `
The list command lists the charms under the given users, by default yours.

   charm list
   charm list -u fred,bob
`

func (c *listCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "list",
		Purpose: "list charms for the given users.",
		Doc:     listDoc,
	}
}

func formatText(w io.Writer, value interface{}) error {
	val := value.([]params.EntityResult)
	if len(val) == 0 {
		fmt.Fprint(w, "No charms found.")
		return nil
	}
	ids := make([]string, len(val))
	for i, result := range val {
		ids[i] = result.Id.String()
	}
	s := strings.Join(ids, "\n")
	fmt.Fprint(w, s)
	return nil
}

func (c *listCommand) SetFlags(f *gnuflag.FlagSet) {
	c.out.AddFlags(f, "text", map[string]cmd.Formatter{
		"yaml": cmd.FormatYaml,
		"json": cmd.FormatJson,
		"text": formatText,
	})
	f.StringVar(&c.users, "u", "", "the given users (comma-separated list)")
	addAuthFlags(f, &c.auth)
}

func (c *listCommand) Init(args []string) error {
	return cmd.CheckEmpty(args)
}

func (c *listCommand) Run(ctxt *cmd.Context) error {
	client, err := newCharmStoreClient(ctxt, c.auth, params.NoChannel)
	if err != nil {
		return errgo.Notef(err, "cannot create charm store client")
	}
	defer client.jar.Save()
	csurl := client.ServerURL()
	storeurl, err := url.Parse(csurl)
	if err != nil {
		return errgo.Notef(err, "invalid URL %q for JUJU_CHARMSTORE", csurl)
	}
	storeurl.Path = strings.TrimSuffix(storeurl.Path, "/") + "/"
	if len(client.jar.Cookies(storeurl)) == 0 {
		fmt.Fprintf(ctxt.Stdout, "not logged into %v\n", csurl)
		return nil
	}
	if c.users == "" {
		resp, err := client.WhoAmI()
		if err != nil {
			return errgo.Notef(err, "cannot retrieve identity")
		}
		c.users = resp.User
	}
	users := strings.Split(c.users, ",")
	err = validateNames(users)
	if err != nil {
		return errgo.Mask(err)
	}
	v := url.Values{
		"sort": []string{"name,-series"},
		"owner": users,
	}
	path := "/list?" + v.Encode()
	var resp params.ListResponse
	err = client.Get(path, &resp)
	if err != nil {
		return errgo.Notef(err, "cannot list for user(s) %s", users)
	}
	return c.out.Write(ctxt, resp.Results)
}
