// Package youtrack is the Panel's only business-data transport. It talks to
// YouTrack, never to a bot, Controller, queue, or execution database.
package youtrack

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const PageSize = 30
const maxResponse = 1 << 20

var databaseID = regexp.MustCompile(`^[0-9]+-[0-9]+$`)
var projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// Errors deliberately never contain URLs, credentials, upstream bodies or user content.
var (
	ErrUnavailable = errors.New("youtrack_unavailable")
	ErrDenied      = errors.New("youtrack_denied")
	ErrInvalid     = errors.New("invalid_request")
	ErrUnknown     = errors.New("write_outcome_unknown")
)

type Project struct {
	ID  string `json:"id"`
	Key string `json:"shortName"`
}

type Field struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

type Issue struct {
	ID          string  `json:"id"`
	ReadableID  string  `json:"idReadable"`
	Summary     string  `json:"summary"`
	Description string  `json:"description"`
	Updated     int64   `json:"updated"`
	Project     Project `json:"project"`
	Fields      []Field `json:"customFields"`
}

type Comment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	Deleted bool   `json:"deleted"`
	Author  struct {
		Name string `json:"name"`
	} `json:"author"`
}

type Article struct {
	ID         string  `json:"id"`
	ReadableID string  `json:"idReadable"`
	Summary    string  `json:"summary"`
	Content    string  `json:"content"`
	Updated    int64   `json:"updated"`
	Project    Project `json:"project"`
}

type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

type Client struct {
	base      *url.URL
	project   Project
	http      *http.Client
	transport *http.Transport
}

// New pins one operator-configured HTTPS endpoint and one project. No proxy
// environment, redirect, caller-supplied URL or sibling runtime is consulted.
func New(endpoint, id, key string) (*Client, error) {
	return NewWithRoots(endpoint, id, key, nil)
}

// NewWithRoots accepts an explicit private CA pool without disabling certificate
// or hostname validation. The executable uses New (system roots) exclusively.
func NewWithRoots(endpoint, id, key string, roots *x509.CertPool) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || !databaseID.MatchString(id) || !projectKey.MatchString(key) {
		return nil, ErrInvalid
	}
	u.Path = ""
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxConnsPerHost: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &Client{base: u, project: Project{ID: id, Key: key}, transport: transport,
		http: &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close()             { c.transport.CloseIdleConnections() }
func (c *Client) Origin() string     { return c.base.String() }
func (c *Client) ProjectKey() string { return c.project.Key }

const issueFields = "id,idReadable,summary,description,updated,project(id,shortName),customFields(name,value(name,id,isResolved))"
const articleFields = "id,idReadable,summary,content,updated,project(id,shortName)"
const commentFields = "id,text,created,deleted,author(name)"

func (c *Client) request(ctx context.Context, token, method, path string, query url.Values, body any, out any) error {
	if len(token) < 16 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return ErrDenied
	}
	u := *c.base
	u.Path = path
	u.RawQuery = query.Encode()
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return ErrInvalid
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404 {
		return ErrDenied
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	ct, _, mimeErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if mimeErr != nil || ct != "application/json" || err != nil || len(data) > maxResponse || !strictjson.Valid(data) || json.Unmarshal(data, out) != nil {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	return nil
}

func fields(value string) url.Values { return url.Values{"fields": {value}} }
func page(value string, skip int) (url.Values, error) {
	if skip < 0 || skip > 100000 {
		return nil, ErrInvalid
	}
	q := fields(value)
	q.Set("$top", strconv.Itoa(PageSize))
	q.Set("$skip", strconv.Itoa(skip))
	return q, nil
}

func (c *Client) Me(ctx context.Context, token string) (User, error) {
	var u User
	err := c.request(ctx, token, http.MethodGet, "/api/users/me", fields("id,login,name"), nil, &u)
	if err == nil && (u.ID == "" || u.Login == "") {
		err = ErrDenied
	}
	return u, err
}

func (c *Client) owns(p Project) bool         { return p.ID == c.project.ID && p.Key == c.project.Key }
func (c *Client) ValidIssue(id string) bool   { return readable(id, c.project.Key+"-") }
func (c *Client) ValidArticle(id string) bool { return readable(id, c.project.Key+"-A-") }
func readable(id, prefix string) bool {
	s, ok := strings.CutPrefix(id, prefix)
	if !ok || len(s) < 1 || len(s) > 12 || s[0] == '0' {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (c *Client) Issues(ctx context.Context, token string, skip int) ([]Issue, error) {
	q, err := page(issueFields, skip)
	if err != nil {
		return nil, err
	}
	q.Set("query", "project: {"+c.project.Key+"} sort by: updated desc")
	var items []Issue
	if err = c.request(ctx, token, http.MethodGet, "/api/issues", q, nil, &items); err != nil {
		return nil, err
	}
	if items == nil || len(items) > PageSize {
		return nil, ErrUnavailable
	}
	for _, i := range items {
		if !c.owns(i.Project) || !c.ValidIssue(i.ReadableID) || !databaseID.MatchString(i.ID) {
			return nil, ErrDenied
		}
	}
	return items, nil
}

func (c *Client) Issue(ctx context.Context, token, id string) (Issue, error) {
	var i Issue
	if !c.ValidIssue(id) {
		return i, ErrDenied
	}
	err := c.request(ctx, token, http.MethodGet, "/api/issues/"+id, fields(issueFields), nil, &i)
	if err == nil && (!c.owns(i.Project) || i.ReadableID != id || !databaseID.MatchString(i.ID)) {
		err = ErrDenied
	}
	return i, err
}

func (c *Client) Comments(ctx context.Context, token, id string, skip int) ([]Comment, error) {
	i, err := c.Issue(ctx, token, id)
	if err != nil {
		return nil, err
	}
	q, err := page(commentFields, skip)
	if err != nil {
		return nil, err
	}
	var items []Comment
	if err = c.request(ctx, token, http.MethodGet, "/api/issues/"+i.ID+"/comments", q, nil, &items); err != nil {
		return nil, err
	}
	if items == nil || len(items) > PageSize {
		return nil, ErrUnavailable
	}
	for _, v := range items {
		if !databaseID.MatchString(v.ID) {
			return nil, ErrUnavailable
		}
	}
	return items, nil
}

func (c *Client) Articles(ctx context.Context, token string, skip int) ([]Article, error) {
	q, err := page(articleFields, skip)
	if err != nil {
		return nil, err
	}
	var items []Article
	if err = c.request(ctx, token, http.MethodGet, "/api/admin/projects/"+c.project.ID+"/articles", q, nil, &items); err != nil {
		return nil, err
	}
	if items == nil || len(items) > PageSize {
		return nil, ErrUnavailable
	}
	for _, a := range items {
		if !c.owns(a.Project) || !c.ValidArticle(a.ReadableID) || !databaseID.MatchString(a.ID) {
			return nil, ErrDenied
		}
	}
	return items, nil
}

func (c *Client) Article(ctx context.Context, token, id string) (Article, error) {
	var a Article
	if !c.ValidArticle(id) {
		return a, ErrDenied
	}
	err := c.request(ctx, token, http.MethodGet, "/api/articles/"+id, fields(articleFields), nil, &a)
	if err == nil && (!c.owns(a.Project) || a.ReadableID != id || !databaseID.MatchString(a.ID)) {
		err = ErrDenied
	}
	return a, err
}

// AddComment records discussion, not an execution command or admission ACK.
// The caller must consume a single-use write permit BEFORE calling this method.
// Never automatically retry it: YouTrack REST does not promise idempotent POST.
func (c *Client) AddComment(ctx context.Context, token, id, text string) (Comment, error) {
	var result Comment
	if strings.TrimSpace(text) == "" || len(text) > 64<<10 {
		return result, ErrInvalid
	}
	i, err := c.Issue(ctx, token, id)
	if err != nil {
		return result, err
	}
	err = c.request(ctx, token, http.MethodPost, "/api/issues/"+i.ID+"/comments", fields(commentFields), map[string]string{"text": text}, &result)
	if err == nil && (!databaseID.MatchString(result.ID) || result.Deleted || result.Text != text) {
		err = ErrUnknown
	}
	return result, err
}
