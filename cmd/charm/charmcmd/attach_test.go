// Copyright 2015 Canonical Ltd.
// Licensed under the GPLv3, see LICENCE file for details.

package charmcmd_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/juju/juju/juju/osenv"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/charm.v6"
	"gopkg.in/juju/charm.v6/resource"
	"gopkg.in/juju/charmrepo.v4/csclient"
	"gopkg.in/juju/charmrepo.v4/csclient/params"
	charmtesting "gopkg.in/juju/charmrepo.v4/testing"

	"github.com/juju/charmstore-client/cmd/charm/charmcmd"
	"net/url"
)

type attachSuite struct {
	commonSuite
}

var _ = gc.Suite(&attachSuite{})

var attachInitErrorTests = []struct {
	args []string
	err  string
}{{
	err: "no charm id specified",
}, {
	args: []string{"wordpress"},
	err:  "no resource specified",
}, {
	args: []string{"wordpress", "foo"},
	err:  "expected name=path format for resource",
}, {
	args: []string{"wordpress", "=foo"},
	err:  "missing resource name",
}, {
	args: []string{"invalid:entity", "foo=bar"},
	err:  `invalid charm id: cannot parse URL "invalid:entity": schema "invalid" not valid`,
}, {
	args: []string{"wordpress", "foo", "something else"},
	err:  "too many arguments",
}}

func (s *attachSuite) TestInitError(c *gc.C) {
	s.discharger.SetDefaultUser("bob")
	dir := c.MkDir()
	for i, test := range attachInitErrorTests {
		c.Logf("test %d: %q", i, test.args)
		args := []string{"attach"}
		stdout, stderr, code := run(dir, append(args, test.args...)...)
		c.Assert(stdout, gc.Equals, "")
		c.Assert(stderr, gc.Matches, "ERROR "+test.err+"\n")
		c.Assert(code, gc.Equals, 2)
	}
}

func (s *attachSuite) TestRun(c *gc.C) {
	s.discharger.SetDefaultUser("bob")
	ch := charmtesting.NewCharmMeta(charmtesting.MetaWithResources(nil, "someResource"))
	id := charm.MustParseURL("~bob/precise/wordpress")
	id, err := s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	dir := c.MkDir()
	err = ioutil.WriteFile(filepath.Join(dir, "bar.zip"), []byte("content"), 0666)
	c.Assert(err, gc.IsNil)

	stdout, stderr, exitCode := run(dir, "attach", "--channel=unpublished", "~bob/precise/wordpress", "someResource=bar.zip")
	c.Check(stdout, gc.Matches, `uploaded revision 0 of someResource\n`)
	c.Check(stderr, gc.Matches, `((\r.*)+\n)?`)
	c.Assert(exitCode, gc.Equals, 0)

	// Check that the resource has actually been attached.
	resources, err := s.client.WithChannel("unpublished").ListResources(id)
	c.Assert(err, gc.IsNil)
	c.Assert(resources, jc.DeepEquals, []params.Resource{{
		Name:        "someResource",
		Type:        "file",
		Path:        "someResource-file",
		Revision:    0,
		Fingerprint: hashOfString("content"),
		Size:        int64(len("content")),
		Description: "someResource description",
	}})
}

func (s *attachSuite) TestResumeUpload(c *gc.C) {
	s.discharger.SetDefaultUser("bob")
	ch := charmtesting.NewCharmMeta(charmtesting.MetaWithResources(nil, "someResource"))
	id := charm.MustParseURL("~bob/precise/wordpress")
	id, err := s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	// Create an upload.
	var uploadInfo params.UploadInfoResponse
	err = s.client.DoWithResponse("POST", "/upload", nil, &uploadInfo)
	c.Assert(err, gc.Equals, nil)

	partSize := uploadInfo.MinPartSize
	// Make some content and fill it with random bytes
	// (random so that the stream isn't compressible, in case
	// net/http is compressing by default).
	content := make([]byte, partSize*2)
	rand.Read(content)

	// Make a proxy so that we can count the amount of
	// traffic being uploaded so we can have assurance
	// that the upload really is being resumed.
	proxy := NewTrafficCounterProxy(c, strings.TrimPrefix(s.srv.URL, "http://"))
	defer proxy.Close()

	proxyURL := "http://" + proxy.Addr()
	client := csclient.New(csclient.Params{
		URL:      proxyURL,
		User:     s.serverParams.AuthUsername,
		Password: s.serverParams.AuthPassword,
	})
	ucacheDir := osenv.JujuXDGDataHomePath("charm-upload-cache")
	ucache := charmcmd.NewUploadIdCache(ucacheDir, time.Hour)
	// Push one part before invoking charm attach.
	putUploadPart(c, client, uploadInfo.UploadId, 1, partSize, partSize*2, content)
	hash := sha256.Sum256(content)

	if n := proxy.WriteCount(); n < partSize || n >= partSize*2 {
		c.Fatalf("proxy write counter not working; got %d want within [%d, %d]", n, partSize, partSize*2-1)
	}
	proxy.ResetCounts()
	c.Assert(proxy.WriteCount(), gc.Equals, int64(0))

	// Update the uploadId cache so that the attach code will see it.
	err = ucache.Update(uploadInfo.UploadId, id, "someResource", hash[:])
	c.Assert(err, gc.Equals, nil)

	*charmcmd.CSClientServerURL = proxyURL
	s.PatchValue(&charmcmd.MinMultipartUploadSize, uploadInfo.MinPartSize)

	dir := c.MkDir()
	err = ioutil.WriteFile(filepath.Join(dir, "bar.zip"), content, 0666)
	c.Assert(err, gc.IsNil)
	stdout, stderr, exitCode := run(dir, "attach", "--channel=unpublished", "~bob/precise/wordpress", "someResource=bar.zip")
	c.Check(stdout, gc.Matches, `uploaded revision 0 of someResource\n`)
	c.Check(stderr, gc.Matches, `resuming previous upload\n(\r.*)+\nfinalizing upload\n`)
	c.Assert(exitCode, gc.Equals, 0)

	if n := proxy.WriteCount(); n > partSize+partSize/2 {
		c.Fatalf("attach did not resume upload; uploaded %.2f%% of expected bytes", float64(n)/float64(partSize)*100)
	}

	// Check that the upload has been removed from
	// the uploadId cache directory.
	entries, err := ioutil.ReadDir(ucacheDir)
	c.Assert(err, gc.Equals, nil)
	c.Assert(entries, gc.HasLen, 0)
}

func (s *attachSuite) TestResumeUploadWithNonexistentUpload(c *gc.C) {
	content := make([]byte, minUploadPartSize*2)
	rand.Read(content)
	hash := sha256.Sum256(content)

	// Create an entry in the uploadId cache that doesn't exist
	// on the server (but otherwise has all the same parameters).
	// This mimics the situation that happens when there's
	// an upload that has expired on the server but remains on disk.
	ucacheDir := osenv.JujuXDGDataHomePath("charm-upload-cache")
	ucache := charmcmd.NewUploadIdCache(ucacheDir, time.Hour)
	id := charm.MustParseURL("~bob/precise/wordpress")
	err := ucache.Update("someid", id, "someResource", hash[:])
	c.Assert(err, gc.Equals, nil)

	ch := charmtesting.NewCharmMeta(charmtesting.MetaWithResources(nil, "someResource"))
	_, err = s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	s.PatchValue(&charmcmd.MinMultipartUploadSize, int64(minUploadPartSize))

	dir := c.MkDir()
	err = ioutil.WriteFile(filepath.Join(dir, "bar.zip"), content, 0666)
	c.Assert(err, gc.IsNil)

	s.discharger.SetDefaultUser("bob")

	_, stderr, exitCode := run(dir, "attach", "--channel=unpublished", "~bob/precise/wordpress", "someResource=bar.zip")
	c.Check(stderr, gc.Matches, `resuming previous upload\n(\r.*)+previous upload seems to have expired; restarting.\n((\r.*)+\n)?finalizing upload\n`)
	c.Assert(exitCode, gc.Equals, 0)

	// Check that the resource has actually been attached.
	resources, err := s.client.WithChannel("unpublished").ListResources(id)
	c.Assert(err, gc.IsNil)
	c.Assert(resources, jc.DeepEquals, []params.Resource{{
		Name:        "someResource",
		Type:        "file",
		Path:        "someResource-file",
		Revision:    0,
		Fingerprint: hashOf(content),
		Size:        int64(len(content)),
		Description: "someResource description",
	}})
}

func (s *attachSuite) TestResumeUploadRemovesExpiredEntries(c *gc.C) {
	ucacheDir := osenv.JujuXDGDataHomePath("charm-upload-cache")
	ucache := charmcmd.NewUploadIdCache(ucacheDir, time.Hour)
	hash := sha256.Sum256(nil)

	// Create the entry. It will have the current time, so then we read
	// it back in and modify it to have a time that's old.
	err := ucache.Update("someid", charm.MustParseURL("~alice/old"), "someResource", hash[:])
	c.Assert(err, gc.Equals, nil)
	entries, err := ioutil.ReadDir(ucacheDir)
	c.Assert(err, gc.Equals, nil)
	c.Assert(entries, gc.HasLen, 1)
	entryPath := filepath.Join(ucacheDir, entries[0].Name())
	data, err := ioutil.ReadFile(entryPath)
	c.Assert(err, gc.Equals, nil)
	var jsonData map[string]interface{}
	err = json.Unmarshal(data, &jsonData)
	c.Assert(err, gc.Equals, nil)
	c.Assert(jsonData["CreatedDate"], gc.FitsTypeOf, "")
	jsonData["CreatedDate"] = time.Now().Add(-200 * 24 * time.Hour)
	data, err = json.Marshal(jsonData)
	c.Assert(err, gc.Equals, nil)
	err = ioutil.WriteFile(entryPath, data, 0666)
	c.Assert(err, gc.Equals, nil)

	content := make([]byte, minUploadPartSize*2)
	rand.Read(content)
	dir := c.MkDir()
	err = ioutil.WriteFile(filepath.Join(dir, "bar.zip"), content, 0666)
	c.Assert(err, gc.IsNil)

	id := charm.MustParseURL("~bob/precise/wordpress")
	ch := charmtesting.NewCharmMeta(charmtesting.MetaWithResources(nil, "someResource"))
	_, err = s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	s.discharger.SetDefaultUser("bob")
	_, stderr, exitCode := run(dir, "attach", "--channel=unpublished", "~bob/precise/wordpress", "someResource=bar.zip")
	c.Assert(exitCode, gc.Equals, 0, gc.Commentf("stderr: %q", stderr))

	_, err = os.Stat(entryPath)

	// The old entry should have been removed.
	c.Assert(err, jc.Satisfies, os.IsNotExist)
}

func (s *attachSuite) TestRunFailsWithoutRevisionOnStableChannel(c *gc.C) {
	s.discharger.SetDefaultUser("bob")
	dir := c.MkDir()
	err := ioutil.WriteFile(filepath.Join(dir, "bar.zip"), []byte("content"), 0666)
	c.Assert(err, gc.IsNil)
	stdout, stderr, exitCode := run(dir, "attach", "--channel=stable", "~bob/precise/wordpress", "someResource=bar.zip")
	c.Assert(exitCode, gc.Equals, 1)
	c.Check(stderr, gc.Matches, "ERROR A revision is required when attaching to a charm in the stable channel.\n")
	c.Check(stdout, gc.Matches, "")
}

func (s *attachSuite) TestCannotOpenFile(c *gc.C) {
	s.discharger.SetDefaultUser("bob")
	id := charm.MustParseURL("~bob/precise/wordpress")
	ch := charmtesting.NewCharmMeta(charmtesting.MetaWithResources(nil, "someResource"))
	_, err := s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	path := filepath.Join(c.MkDir(), "/not-there")
	stdout, stderr, exitCode := run(c.MkDir(), "attach", "~bob/precise/wordpress-0", "someResource="+path)
	c.Assert(exitCode, gc.Equals, 1)
	c.Assert(stdout, gc.Equals, "")
	c.Assert(stderr, gc.Matches, `ERROR open .*not-there: no such file or directory`+"\n")
}

func (s *attachSuite) TestUploadDockerResource(c *gc.C) {
	s.discharger.SetDefaultUser("bob")

	id := charm.MustParseURL("~bob/wordpress")
	ch := charmtesting.NewCharmMeta(&charm.Meta{
		Series: []string{"kubernetes"},
		Resources: map[string]resource.Meta{
			"docker-resource": {
				Name: "docker-resource",
				Type: resource.TypeDocker,
			},
		},
	})
	_, err := s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	stdout, stderr, exitCode := run(c.MkDir(), "attach", "~bob/wordpress-0", "docker-resource=some/docker/imagename")
	c.Assert(exitCode, gc.Equals, 0, gc.Commentf("stdout: %q; stderr: %q", stdout, stderr))
	c.Assert(stdout, gc.Equals, "uploaded revision 0 of docker-resource\n")
	c.Assert(stderr, gc.Equals, "")

	imageName := s.dockerHost + "/bob/wordpress/docker-resource"
	c.Assert(s.dockerHandler.reqs, jc.DeepEquals, []interface{}{
		tagRequest{
			imageID: "docker.io/some/docker/imagename",
			tag:     "latest",
			repo:    imageName,
		},
		pushRequest{
			imageID: imageName,
		},
	})
	info, err := s.client.DockerResourceDownloadInfo(id.WithRevision(0), "docker-resource")
	c.Assert(err, gc.Equals, nil)
	c.Assert(info.ImageName, gc.Equals, imageName+"@"+s.dockerHandler.imageDigest(imageName))
}

func (s *attachSuite) TestUploadExternalDockerResource(c *gc.C) {
	s.discharger.SetDefaultUser("bob")

	id := charm.MustParseURL("~bob/wordpress")
	ch := charmtesting.NewCharmMeta(&charm.Meta{
		Series: []string{"kubernetes"},
		Resources: map[string]resource.Meta{
			"docker-resource": {
				Name: "docker-resource",
				Type: resource.TypeDocker,
			},
		},
	})
	_, err := s.client.UploadCharm(id, ch)
	c.Assert(err, gc.IsNil)

	u, err := url.Parse(s.dockerRegistry.URL)
	c.Assert(err, gc.IsNil)

	_, stderr, exitCode := run(c.MkDir(), "attach", "~bob/wordpress-0", "docker-resource=external::"+ u.Host +"/foo/bar")
	c.Assert(exitCode, gc.Equals, 1)
	c.Assert(stderr, gc.Matches, `ERROR external images not yet supported\n`)
}

func putUploadPart(c *gc.C, client *csclient.Client, uploadId string, partIndex int, p0, p1 int64, content []byte) {
	partContent := content[p0:p1]
	hash := sha512.Sum384([]byte(partContent))
	req, err := http.NewRequest("PUT", "", bytes.NewReader(partContent))
	c.Assert(err, gc.Equals, nil)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = p1 - p0
	resp, err := client.Do(req, fmt.Sprintf("/upload/%s/%d?hash=%x&offset=%d", uploadId, partIndex, hash, p0))
	c.Assert(err, gc.Equals, nil)
	resp.Body.Close()
}

// TrafficCounterProxy is a TCP proxy that counts the
// number of bytes transferred.
type TrafficCounterProxy struct {
	listener   net.Listener
	remoteAddr string
	writeCount int64
	readCount  int64
}

// NewTrafficCounter runs a proxy that copies to and from
// the given remote TCP address. It should be closed after
// use.
func NewTrafficCounterProxy(c *gc.C, remoteAddr string) *TrafficCounterProxy {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	c.Assert(err, jc.ErrorIsNil)
	p := &TrafficCounterProxy{
		listener:   listener,
		remoteAddr: remoteAddr,
	}
	go p.run(c)
	return p
}

func (p *TrafficCounterProxy) run(c *gc.C) {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.remoteAddr)
		if err != nil {
			c.Errorf("cannot dial remote address: %v", err)
			return
		}
		go p.stream(client, server, &p.readCount)
		go p.stream(server, client, &p.writeCount)
	}
}

func (p *TrafficCounterProxy) Close() {
	p.listener.Close()
}

// Addr returns the TCP address of the proxy. Dialing
// this address will cause a connection to be made
// to the remote address; any data written will be
// written there, and any data read from the remote
// address will be available to read locally.
func (p *TrafficCounterProxy) Addr() string {
	// Note: this only works because we explicitly listen on 127.0.0.1 rather
	// than the wildcard address.
	return p.listener.Addr().String()
}

func (p *TrafficCounterProxy) ResetCounts() {
	atomic.StoreInt64(&p.readCount, 0)
	atomic.StoreInt64(&p.writeCount, 0)
}

// ReadCount returns the number of bytes read by clients.
func (p *TrafficCounterProxy) ReadCount() int64 {
	return atomic.LoadInt64(&p.readCount)
}

// WriteCount returns the number of bytes written by clients to the
// server.
func (p *TrafficCounterProxy) WriteCount() int64 {
	return atomic.LoadInt64(&p.writeCount)
}

func (p *TrafficCounterProxy) stream(dst io.WriteCloser, src io.ReadCloser, counter *int64) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32*1024)
	for {
		nr, err := src.Read(buf)
		if nr > 0 {
			atomic.AddInt64(counter, int64(nr))
			_, err := dst.Write(buf[0:nr])
			if err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
}

func hashOfString(s string) []byte {
	return hashOf([]byte(s))
}

func hashOf(b []byte) []byte {
	x := sha512.Sum384(b)
	return x[:]
}
