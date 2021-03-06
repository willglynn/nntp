// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The nntp package implements a client for the news protocol NNTP,
// as defined in RFC 3977.
package nntp

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// timeFormatNew is the NNTP time format string for NEWNEWS / NEWGROUPS
const timeFormatNew = "20060102 150405"

// timeFormatDate is the NNTP time format string for responses to the DATE command
const timeFormatDate = "20060102150405"

// An Error represents an error response from an NNTP server.
type Error struct {
	Code uint
	Msg  string
}

// A ProtocolError represents responses from an NNTP server
// that seem incorrect for NNTP.
type ProtocolError string

// A Conn represents a connection to an NNTP server. The connection with
// an NNTP server is stateful; it keeps track of what group you have
// selected, if any, and (if you have a group selected) which article is
// current, next, or previous.
//
// Some methods that return information about a specific message take
// either a message-id, which is global across all NNTP servers, groups,
// and messages, or a message-number, which is an integer number that is
// local to the NNTP session and currently selected group.
//
// For all methods that return an io.Reader (or an *Article, which contains
// an io.Reader), that io.Reader is only valid until the next call to a
// method of Conn.
type Conn struct {
	conn   io.ReadWriteCloser
	w      io.Writer
	r      *bufio.Reader
	br     *bodyReader
	close  bool
	quirks struct {
		xzverUnsupported bool
		xzverSupported   bool
	}
}

// A Group gives information about a single news group on the server.
type Group struct {
	Name string
	// Count of messages in the group
	Count int64
	// High and low message-numbers
	High, Low int64
	// Status indicates if general posting is allowed --
	// typical values are "y", "n", or "m".
	Status string
}

// An Article represents an NNTP article.
type Article struct {
	Header map[string][]string
	Body   io.Reader
}

// A bodyReader satisfies reads by reading from the connection
// until it finds a line containing just .
type bodyReader struct {
	c   *Conn
	eof bool
	buf *bytes.Buffer
}

var dotnl = []byte(".\n")
var dotdot = []byte("..")

func (r *bodyReader) Read(p []byte) (n int, err error) {
	if r.eof {
		return 0, io.EOF
	}
	if r.buf == nil {
		r.buf = &bytes.Buffer{}
	}
	if r.buf.Len() == 0 {
		b, err := r.c.r.ReadBytes('\n')
		if err != nil {
			return 0, err
		}
		// canonicalize newlines
		if b[len(b)-2] == '\r' { // crlf->lf
			b = b[0 : len(b)-1]
			b[len(b)-1] = '\n'
		}
		// stop on .
		if bytes.Equal(b, dotnl) {
			r.eof = true
			return 0, io.EOF
		}
		// unescape leading ..
		if bytes.HasPrefix(b, dotdot) {
			b = b[1:]
		}
		r.buf.Write(b)
	}
	n, _ = r.buf.Read(p)
	return
}

func (r *bodyReader) discard() error {
	_, err := ioutil.ReadAll(r)
	return err
}

// articleReader satisfies reads by dumping out an article's headers
// and body.
type articleReader struct {
	a          *Article
	headerdone bool
	headerbuf  *bytes.Buffer
}

func (r *articleReader) Read(p []byte) (n int, err error) {
	if r.headerbuf == nil {
		buf := new(bytes.Buffer)
		for k, fv := range r.a.Header {
			for _, v := range fv {
				fmt.Fprintf(buf, "%s: %s\n", k, v)
			}
		}
		if r.a.Body != nil {
			fmt.Fprintf(buf, "\n")
		}
		r.headerbuf = buf
	}
	if !r.headerdone {
		n, err = r.headerbuf.Read(p)
		if err == io.EOF {
			err = nil
			r.headerdone = true
		}
		if n > 0 {
			return
		}
	}
	if r.a.Body != nil {
		n, err = r.a.Body.Read(p)
		if err == io.EOF {
			r.a.Body = nil
		}
		return
	}
	return 0, io.EOF
}

func (a *Article) String() string {
	id, ok := a.Header["Message-Id"]
	if !ok {
		return "[NNTP article]"
	}
	return fmt.Sprintf("[NNTP article %s]", id[0])
}

func (a *Article) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, &articleReader{a: a})
}

func IsProtocol(err error) bool {
	_, ok := err.(ProtocolError)
	return ok
}

func ErrorCode(err error) uint {
	if nntpErr, ok := err.(Error); ok {
		return nntpErr.Code
	}
	return 0
}

func (p ProtocolError) Error() string {
	return string(p)
}

func (e Error) Error() string {
	return fmt.Sprintf("%03d %s", e.Code, e.Msg)
}

func maybeId(cmd, id string) string {
	if len(id) > 0 {
		return cmd + " " + id
	}
	return cmd
}

func newConn(c net.Conn) (res *Conn, err error) {
	res = &Conn{
		conn: c,
		w:    c,
		r:    bufio.NewReaderSize(c, 4096),
	}

	if _, err = res.r.ReadString('\n'); err != nil {
		return
	}

	return
}

// Dial connects to an NNTP server.
// The network and addr are passed to net.Dial to
// make the connection.
//
// Example:
//   conn, err := nntp.Dial("tcp", "my.news:nntp")
//
func Dial(network, addr string) (*Conn, error) {
	c, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	return newConn(c)
}

// Same as Dial but handles TLS connections
func DialTLS(network, addr string, config *tls.Config) (*Conn, error) {
	// dial
	c, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	// handshake TLS
	c = tls.Client(c, config)
	if err = c.(*tls.Conn).Handshake(); err != nil {
		return nil, err
	}
	// should we check cert
	if config == nil || !config.InsecureSkipVerify {
		// get host name
		host := strings.SplitN(addr, ":", 2)
		// check valid cert for host
		if err = c.(*tls.Conn).VerifyHostname(host[0]); err != nil {
			return nil, err
		}
	}
	// return nntp Conn
	return newConn(c)
}

// Enables tracing, such that future IO gets dumped to the indicated writers,
// replacing any current tracing configuration.
//
// This discards the contents of the current server-to-client receive buffer
// and should therefore not be attempted while any commands are in progress.
func (c *Conn) Trace(c2s, s2c io.Writer) {
	if c2s != nil {
		c.w = io.MultiWriter(c.conn, c2s)
	} else {
		c.w = c.conn
	}

	if s2c != nil {
		c.r.Reset(io.TeeReader(c.conn, s2c))
	} else {
		c.r.Reset(c.conn)
	}
}

func (c *Conn) body() io.Reader {
	c.br = &bodyReader{c: c}
	return c.br
}

// readStrings reads a list of strings from the NNTP connection,
// stopping at a line containing only a . (Convenience method for
// LIST, etc.)
func readStrings(r *bufio.Reader) ([]string, error) {
	var sv []string
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if strings.HasSuffix(line, "\r\n") {
			line = line[0 : len(line)-2]
		} else if strings.HasSuffix(line, "\n") {
			line = line[0 : len(line)-1]
		}
		if line == "." {
			break
		}
		sv = append(sv, line)
	}
	return sv, nil
}

// Authenticate logs in to the NNTP server.
// It only sends the password if the server requires one.
func (c *Conn) Authenticate(username, password string) error {
	code, _, err := c.cmd(2, "AUTHINFO USER %s", username)
	if code/100 == 3 {
		_, _, err = c.cmd(2, "AUTHINFO PASS %s", password)
	}
	return err
}

// cmd executes an NNTP command:
// It sends the command given by the format and arguments, and then
// reads the response line. If expectCode > 0, the status code on the
// response line must match it. 1 digit expectCodes only check the first
// digit of the status code, etc.
func (c *Conn) cmd(expectCode uint, format string, args ...interface{}) (code uint, line string, err error) {
	if c.close {
		return 0, "", ProtocolError("connection closed")
	}
	if c.br != nil {
		if err := c.br.discard(); err != nil {
			return 0, "", err
		}
		c.br = nil
	}
	if _, err := fmt.Fprintf(c.w, format+"\r\n", args...); err != nil {
		return 0, "", err
	}
	line, err = c.r.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimSpace(line)
	if len(line) < 4 || line[3] != ' ' {
		return 0, "", ProtocolError(fmt.Sprintf("short response: %+q", line))
	}
	i, err := strconv.ParseUint(line[0:3], 10, 0)
	if err != nil {
		return 0, "", ProtocolError("invalid response code: " + line)
	}
	code = uint(i)
	line = line[4:]
	if 1 <= expectCode && expectCode < 10 && code/100 != expectCode ||
		10 <= expectCode && expectCode < 100 && code/10 != expectCode ||
		100 <= expectCode && expectCode < 1000 && code != expectCode {
		err = Error{code, line}
	}
	return
}

// ModeReader switches the NNTP server to "reader" mode, if it
// is a mode-switching server.
func (c *Conn) ModeReader() error {
	_, _, err := c.cmd(20, "MODE READER")
	return err
}

// NewGroups returns a list of groups added since the given time.
func (c *Conn) NewGroups(since time.Time) ([]*Group, error) {
	if _, line, err := c.cmd(231, "NEWGROUPS %s GMT", since.Format(timeFormatNew)); err != nil {
		return nil, err
	} else {
		return c.readGroups(line)
	}
}

func (c *Conn) readGroups(line string) ([]*Group, error) {
	var lines []string
	var err error

	if strings.Contains(line, "[COMPRESS=GZIP]") {
		zdr, err := newZlibDotResponse(c.r)
		defer zdr.Close()

		if err == nil {
			lines, err = readStrings(zdr.Reader)
		}

		if err == nil {
			err = zdr.Close()
		}

	} else {
		lines, err = readStrings(c.r)
	}

	if err != nil {
		return nil, err
	}

	return parseGroups(lines)
}

// NewNews returns a list of the IDs of articles posted
// to the given group since the given time.
func (c *Conn) NewNews(group string, since time.Time) ([]string, error) {
	if _, _, err := c.cmd(230, "NEWNEWS %s %s GMT", group, since.Format(timeFormatNew)); err != nil {
		return nil, err
	}

	id, err := readStrings(c.r)
	if err != nil {
		return nil, err
	}

	sort.Strings(id)
	w := 0
	for r, s := range id {
		if r == 0 || id[r-1] != s {
			id[w] = s
			w++
		}
	}
	id = id[0:w]

	return id, nil
}

// Overview of a message returned by OVER command.
type MessageOverview struct {
	MessageNumber int64     // Message number in the group
	Subject       string    // Subject header value. Empty if the header is missing.
	From          string    // From header value. Empty is the header is missing.
	Date          time.Time // Parsed Date header value. Zero if the header is missing or unparseable.
	MessageId     string    // Message-Id header value. Empty is the header is missing.
	References    []string  // Message-Id's of referenced messages (References header value, split on spaces). Empty if the header is missing.
	Bytes         int       // Message size in bytes, called :bytes metadata item in RFC3977.
	Lines         int       // Message size in lines, called :lines metadata item in RFC3977.
	Extra         []string  // Any additional fields returned by the server.
}

// Overview returns overviews of all messages in the current group with message number between
// begin and end, inclusive.
func (c *Conn) Overview(begin, end int64) ([]MessageOverview, error) {
	if !c.quirks.xzverUnsupported || c.quirks.xzverSupported {
		// Try XZVERing: http://helpdesk.astraweb.com/index.php?_m=news&_a=viewnews&newsid=9
		if _, _, xzerr := c.cmd(224, "XZVER %d-%d", begin, end); xzerr != nil {
			c.quirks.xzverUnsupported = true
		} else {
			c.quirks.xzverSupported = true
			return c.parseXzver()
		}
	}

	var line string
	var err, xerr error
	if _, line, err = c.cmd(224, "OVER %d-%d", begin, end); err != nil {
		if nerr, ok := err.(Error); ok && nerr.Code == 500 {
			// This could mean that OVER isn't supported.
			// Attempt XOVER instead.
			if _, line, xerr = c.cmd(224, "XOVER %d-%d", begin, end); xerr != nil {
				// XOVER failed too. Return the original error.
				return nil, err
			}
		} else {
			// Some other type of error.
			return nil, err
		}
	}

	// if we're using XFEATURE COMPRESS GZIP, the response line seems to contain this magic string
	// (I wish I had a spec for this…)
	if strings.Contains(line, "[COMPRESS=GZIP]") {
		zdr, err := newZlibDotResponse(c.r)
		defer zdr.Close()

		var msgs []MessageOverview
		if err == nil {
			msgs, err = parseOverview(zdr.Reader)
		}

		if err == nil {
			err = zdr.Close()
		}

		if err != nil {
			return nil, err
		} else {
			return msgs, nil
		}

	} else {
		// plain response
		return parseOverview(c.r)
	}
}

func (c *Conn) parseXzver() (result []MessageOverview, err error) {
	// XZVER is a yenc stream…
	yencStream := &yencReader{r: c.r}
	defer yencStream.Close()

	// containing a DEFLATE stream…
	flateStream := flate.NewReader(yencStream)
	defer flateStream.Close()

	// containing an overview stream…
	result, err = parseOverview(bufio.NewReader(flateStream))

	if err == nil {
		// …with a dot at the end
		flateStream.Close()
		yencStream.Close()

		var line string
		line, err = c.r.ReadString('\n')
		if err == nil && strings.TrimRight(line, "\r\n") != "." {
			return nil, fmt.Errorf("unexpected data after XZVER: %q", line)
		}
	}

	return
}

func parseOverview(r *bufio.Reader) ([]MessageOverview, error) {
	result := make([]MessageOverview, 0)

	for {
		line, err := r.ReadString('\n')
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return result, nil
		} else if err != nil {
			return nil, err
		}
		if strings.HasSuffix(line, "\r\n") {
			line = line[0 : len(line)-2]
		} else if strings.HasSuffix(line, "\n") {
			line = line[0 : len(line)-1]
		}

		if line == "." {
			break
		}

		overview := MessageOverview{}
		ss := strings.Split(strings.TrimSpace(line), "\t")
		if len(ss) < 8 {
			return nil, ProtocolError("short header listing line: " + line + strconv.Itoa(len(ss)))
		}
		overview.MessageNumber, err = strconv.ParseInt(ss[0], 10, 64)
		if err != nil {
			return nil, ProtocolError("bad message number '" + ss[0] + "' in line: " + line)
		}
		overview.Subject = ss[1]
		overview.From = ss[2]
		overview.Date, err = parseDate(ss[3])
		if err != nil {
			// Inability to parse date is not fatal: the field in the message may be broken or missing.
			overview.Date = time.Time{}
		}
		overview.MessageId = ss[4]

		// At least one server in the wild returns tab delimited references. This sucks.
		//
		// As a hack: as long as ss[6] isn't parseable as number of bytes and there are extra
		// tab-delimited fields, assume ss[6] is a continuation of ss[5], and glue the fields
		// together.
		//
		// This doesn't break anything on "normal" servers and works around this particular
		// failure mode.
		for {
			if len(ss) < 8 {
				break
			}

			overview.Bytes, err = strconv.Atoi(ss[6])
			if err != nil {
				ss[5] = ss[5] + ss[6]
				ss = append(ss[:6], ss[7:]...)
			} else {
				break
			}
		}

		overview.References = strings.Split(ss[5], " ") // Message-Id's contain no spaces, so this is safe.
		if ss[7] == "" {
			overview.Lines = 0 // unspecified
		} else if overview.Lines, err = strconv.Atoi(ss[7]); err != nil {
			return nil, ProtocolError(fmt.Sprintf("bad line count %q in line %q (split into %#v)", ss[7], line, ss)) // eww, string formatting
		}
		overview.Extra = append([]string{}, ss[8:]...)
		result = append(result, overview)
	}

	return result, nil
}

// parseGroups is used to parse a list of group states.
func parseGroups(lines []string) ([]*Group, error) {
	res := make([]*Group, 0)
	for _, line := range lines {
		ss := strings.SplitN(strings.TrimSpace(line), " ", 4)
		if len(ss) < 4 {
			return nil, ProtocolError("short group info line: " + line)
		}
		high, err := strconv.ParseInt(ss[1], 10, 64)
		if err != nil {
			return nil, ProtocolError("bad number in line: " + line)
		}
		low, err := strconv.ParseInt(ss[2], 10, 64)
		if err != nil {
			return nil, ProtocolError("bad number in line: " + line)
		}
		res = append(res, &Group{Name: ss[0], High: high, Low: low, Status: ss[3]})
	}
	return res, nil
}

// Capabilities returns a list of features this server performs.
// Not all servers support capabilities.
func (c *Conn) Capabilities() ([]string, error) {
	if _, _, err := c.cmd(101, "CAPABILITIES"); err != nil {
		return nil, err
	}
	return readStrings(c.r)
}

func (c *Conn) ListExtensions() ([]string, error) {
	if _, _, err := c.cmd(202, "LIST EXTENSIONS"); err != nil {
		return nil, err
	}
	return readStrings(c.r)
}

// Date returns the current time on the server.
// Typically the time is later passed to NewGroups or NewNews.
func (c *Conn) Date() (time.Time, error) {
	_, line, err := c.cmd(111, "DATE")
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(timeFormatDate, line)
	if err != nil {
		return time.Time{}, ProtocolError("invalid time: " + line)
	}
	return t, nil
}

// Attempt to enable connection-level compression, e.g. XFEATURE COMPRESS GZIP.
// This will fail with an nntp.Error if the server doesn't support it, or some other
// type of error in case something more severe has occurred.
func (c *Conn) EnableCompression() error {
	_, _, err := c.cmd(290, "XFEATURE COMPRESS GZIP")
	return err
}

// List returns a list of groups present on the server.
// Valid forms are:
//
//   List() - return active groups
//   List(keyword) - return different kinds of information about groups
//   List(keyword, pattern) - filter groups against a glob-like pattern called a wildmat
//
func (c *Conn) List(a ...string) ([]*Group, error) {
	if len(a) > 2 {
		return nil, ProtocolError("List only takes up to 2 arguments")
	}
	cmd := "LIST"
	if len(a) > 0 {
		cmd += " " + a[0]
		if len(a) > 1 {
			cmd += " " + a[1]
		}
	}
	if _, line, err := c.cmd(215, cmd); err != nil {
		return nil, err
	} else {
		return c.readGroups(line)
	}
}

// Group changes the current group.
func (c *Conn) Group(group string) (status *Group, err error) {
	_, line, err := c.cmd(211, "GROUP %s", group)
	if err != nil {
		return
	}

	status, err = parseGroupStatus(line)
	if err != nil {
		status.Name = group
	}
	return
}

func parseGroupStatus(line string) (status *Group, err error) {
	ss := strings.SplitN(line, " ", 4) // intentional -- we ignore optional message
	if len(ss) < 3 {
		err = ProtocolError("bad group response: " + line)
		return
	}

	var n [3]int64
	for i, _ := range n {
		c, e := strconv.ParseInt(ss[i], 10, 64)
		if e != nil {
			err = ProtocolError("bad group response: " + line)
			return
		}
		n[i] = c
	}
	status = &Group{Count: n[0], Low: n[1], High: n[2]}
	return
}

type GroupListing struct {
	Group
	Articles []int64
}

// ListGroup changes the current group.
func (c *Conn) ListGroup(group string, from, to int64) (listing *GroupListing, err error) {
	cmd := fmt.Sprintf("LISTGROUP %s", group)
	if from >= 0 {
		cmd = fmt.Sprintf("%s %d-", cmd, from)
	}
	if to >= 0 {
		cmd = fmt.Sprintf("%s%d", cmd, to)
	}
	fmt.Println(cmd)

	_, line, err := c.cmd(211, cmd)
	if err != nil {
		return
	}
	ss := strings.SplitN(line, " ", 4)
	var status *Group
	if len(ss) >= 3 {
		status, err = parseGroupStatus(line)
		if err != nil {
			return
		}
		status.Name = group
	} else {
		status = &Group{Name: group}
	}

	listing = &GroupListing{Group: *status}

	br := bufio.NewReader(c.body())
	eof := false
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			eof = true
		} else if err != nil {
			return nil, err
		}
		if eof && len(line) == 0 {
			break
		}
		line = strings.TrimSpace(line)
		if line == "." {
			break
		}
		num, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			// TODO: warn but do not fail on a bulk operation
			return nil, err
		}
		listing.Articles = append(listing.Articles, num)
	}

	return
}

// Help returns the server's help text.
func (c *Conn) Help() (io.Reader, error) {
	if _, _, err := c.cmd(100, "HELP"); err != nil {
		return nil, err
	}
	return c.body(), nil
}

// nextLastStat performs the work for NEXT, LAST, and STAT.
func (c *Conn) nextLastStat(cmd, id string) (string, string, error) {
	_, line, err := c.cmd(223, maybeId(cmd, id))
	if err != nil {
		return "", "", err
	}
	ss := strings.SplitN(line, " ", 3) // optional comment ignored
	if len(ss) < 2 {
		return "", "", ProtocolError("Bad response to " + cmd + ": " + line)
	}
	return ss[0], ss[1], nil
}

// Stat looks up the message with the given id and returns its
// message number in the current group, and vice versa.
// The returned message number can be "0" if the current group
// isn't one of the groups the message was posted to.
func (c *Conn) Stat(id string) (number, msgid string, err error) {
	return c.nextLastStat("STAT", id)
}

// Last selects the previous article, returning its message number and id.
func (c *Conn) Last() (number, msgid string, err error) {
	return c.nextLastStat("LAST", "")
}

// Next selects the next article, returning its message number and id.
func (c *Conn) Next() (number, msgid string, err error) {
	return c.nextLastStat("NEXT", "")
}

// ArticleText returns the article named by id as an io.Reader.
// The article is in plain text format, not NNTP wire format.
func (c *Conn) ArticleText(id string) (io.Reader, error) {
	if _, _, err := c.cmd(220, maybeId("ARTICLE", id)); err != nil {
		return nil, err
	}
	return c.body(), nil
}

// Article returns the article named by id as an *Article.
func (c *Conn) Article(id string) (*Article, error) {
	if _, _, err := c.cmd(220, maybeId("ARTICLE", id)); err != nil {
		return nil, err
	}
	r := bufio.NewReader(c.body())
	res, err := c.readHeader(r)
	if err != nil {
		return nil, err
	}
	res.Body = r
	return res, nil
}

// HeadText returns the header for the article named by id as an io.Reader.
// The article is in plain text format, not NNTP wire format.
func (c *Conn) HeadText(id string) (io.Reader, error) {
	if _, _, err := c.cmd(221, maybeId("HEAD", id)); err != nil {
		return nil, err
	}
	return c.body(), nil
}

// Head returns the header for the article named by id as an *Article.
// The Body field in the Article is nil.
func (c *Conn) Head(id string) (*Article, error) {
	if _, _, err := c.cmd(221, maybeId("HEAD", id)); err != nil {
		return nil, err
	}
	return c.readHeader(bufio.NewReader(c.body()))
}

// Body returns the body for the article named by id as an io.Reader.
func (c *Conn) Body(id string) (io.Reader, error) {
	if _, _, err := c.cmd(222, maybeId("BODY", id)); err != nil {
		return nil, err
	}
	return c.body(), nil
}

// RawPost reads a text-formatted article from r and posts it to the server.
func (c *Conn) RawPost(r io.Reader) error {
	if _, _, err := c.cmd(3, "POST"); err != nil {
		return err
	}
	br := bufio.NewReader(r)
	eof := false
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			eof = true
		} else if err != nil {
			return err
		}
		if eof && len(line) == 0 {
			break
		}
		if strings.HasSuffix(line, "\n") {
			line = line[0 : len(line)-1]
		}
		var prefix string
		if strings.HasPrefix(line, ".") {
			prefix = "."
		}
		_, err = fmt.Fprintf(c.w, "%s%s\r\n", prefix, line)
		if err != nil {
			return err
		}
		if eof {
			break
		}
	}

	if _, _, err := c.cmd(240, "."); err != nil {
		return err
	}
	return nil
}

// Post posts an article to the server.
func (c *Conn) Post(a *Article) error {
	return c.RawPost(&articleReader{a: a})
}

// Quit sends the QUIT command and closes the connection to the server.
func (c *Conn) Quit() error {
	_, _, err := c.cmd(0, "QUIT")
	c.conn.Close()
	c.close = true
	return err
}

// Functions after this point are mostly copy-pasted from http
// (though with some modifications). They should be factored out to
// a common library.

// Read a line of bytes (up to \n) from b.
// Give up if the line exceeds maxLineLength.
// The returned bytes are a pointer into storage in
// the bufio, so they are only valid until the next bufio read.
func readLineBytes(b *bufio.Reader) (p []byte, err error) {
	if p, err = b.ReadSlice('\n'); err != nil {
		// We always know when EOF is coming.
		// If the caller asked for a line, there should be a line.
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	// Chop off trailing white space.
	var i int
	for i = len(p); i > 0; i-- {
		if c := p[i-1]; c != '\r' && c != '\t' && c != '\n' {
			break
		}
	}
	return p[0:i], nil
}

var colon = []byte{':'}

// Read a key/value pair from b.
// A key/value has the form Key: Value\r\n
// and the Value can continue on multiple lines if each continuation line
// starts with a space/tab.
func readKeyValue(b *bufio.Reader) (key, value string, err error) {
	line, e := readLineBytes(b)
	if e == io.ErrUnexpectedEOF {
		return "", "", nil
	} else if e != nil {
		return "", "", e
	}
	if len(line) == 0 {
		return "", "", nil
	}

	// Scan first line for colon.
	i := bytes.Index(line, colon)
	if i < 0 {
		goto Malformed
	}

	key = string(line[0:i])
	if strings.Index(key, " ") >= 0 {
		// Key field has space - no good.
		goto Malformed
	}

	// Skip initial space before value.
	for i++; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			break
		}
	}
	value = string(line[i:])

	// Look for extension lines, which must begin with space.
	for {
		c, e := b.ReadByte()
		if c != ' ' && c != '\t' {
			if e != io.EOF {
				b.UnreadByte()
			}
			break
		}

		// Eat leading space.
		for c == ' ' || c == '\t' {
			if c, e = b.ReadByte(); e != nil {
				if e == io.EOF {
					e = io.ErrUnexpectedEOF
				}
				return "", "", e
			}
		}
		b.UnreadByte()

		// Read the rest of the line and add to value.
		if line, e = readLineBytes(b); e != nil {
			return "", "", e
		}
		value += " " + string(line)
	}
	return key, value, nil

Malformed:
	return "", "", ProtocolError("malformed header line: " + string(line))
}

// Internal. Parses headers in NNTP articles. Most of this is stolen from the http package,
// and it should probably be split out into a generic RFC822 header-parsing package.
func (c *Conn) readHeader(r *bufio.Reader) (res *Article, err error) {
	res = new(Article)
	res.Header = make(map[string][]string)
	for {
		var key, value string
		if key, value, err = readKeyValue(r); err != nil {
			return nil, err
		}
		if key == "" {
			break
		}
		key = http.CanonicalHeaderKey(key)
		// RFC 3977 says nothing about duplicate keys' values being equivalent to
		// a single key joined with commas, so we keep all values seperate.
		oldvalue, present := res.Header[key]
		if present {
			sv := make([]string, 0)
			sv = append(sv, oldvalue...)
			sv = append(sv, value)
			res.Header[key] = sv
		} else {
			res.Header[key] = []string{value}
		}
	}
	return res, nil
}
