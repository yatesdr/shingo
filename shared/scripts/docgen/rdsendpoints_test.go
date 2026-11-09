package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rdsepFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, "shingo-core", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

var rdsepFixtureFiles = map[string]string{
	"rds/client.go": `package rds

func (c *Client) get(path string, out any) error  { return nil }
func (c *Client) post(path string, in, out any) error { return nil }

func (c *Client) Ping() error { return c.get("/ping", nil) }
func (c *Client) Detail(id string) error { return c.get(fmt.Sprintf("/orderDetails/%s", id), nil) }
func (c *Client) Relay() error {
	u := c.url("/relay")
	_, err := hc.Post(u, "application/json", nil)
	return err
}
func (c *Client) Laser() error { return c.Relay() }
func (c *Client) Lic() error { return c.get("/licInfo", nil) }
func (c *Client) Bins(groups []string) error {
	path := "/binDetails"
	if len(groups) > 0 {
		path = fmt.Sprintf("/binDetails?binGroups=%s", strings.Join(groups, ","))
	}
	return c.get(path, nil)
}
`,
	"rds/poller.go": `package rds

func NewPoller(c *Client) *Poller { return &Poller{client: c} }
func (p *Poller) Start() { go p.run() }
func (p *Poller) run() { p.client.Detail("x") }
`,
	"rds/client_test.go": `package rds

func (c *Client) TestOnly() error { return c.get("/testOnly", nil) }
`,
	"fleet/seerrds/adapter.go": `package seerrds

func New() *Adapter {
	client := rds.NewClient("", 0)
	return &Adapter{client: client, poller: rds.NewPoller(client)}
}
func (a *Adapter) Up() error { a.poller.Start(); return a.client.Ping() }
func (a *Adapter) Lidar() error { return a.client.Laser() }
`,
}

// Reachability, pinned on rows that only say yes if it fired: a direct call
// on the adapter's client, a func reached only through the poller, a func
// reached only through another wrapper; and the endpoint shapes: a path built
// by Sprintf or held in a variable is its name, a test file's func is not a
// client func.
func TestRdsep_IndexFromFixture(t *testing.T) {
	got, err := rdsepEmit(rdsepFixture(t, rdsepFixtureFiles))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"5 endpoints through 5 `rds` client funcs; the fleet adapter reaches 3 of the funcs (3 endpoints).",
		"| `/binDetails` | GET | `Client.Bins` | no |",
		"| `/licInfo` | GET | `Client.Lic` | no |",
		"| `/orderDetails` | GET | `Client.Detail` | yes |",
		"| `/ping` | GET | `Client.Ping` | yes |",
		"| `/relay` | POST | `Client.Relay` | yes |",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s\n--- got:\n%s", w, got)
		}
	}
	if strings.Contains(got, "testOnly") {
		t.Errorf("a _test.go func reached the index:\n%s", got)
	}
}

// An endpoint literal in a func that calls no HTTP helper the parse knows is
// an error, not a row with a blank method.
func TestRdsep_RefusesAnEndpointWithNoMethod(t *testing.T) {
	files := map[string]string{
		"rds/odd.go":         "package rds\n\nfunc (c *Client) Odd() error { return c.send(\"/odd\") }\n",
		"fleet/seerrds/a.go": "package seerrds\n",
	}
	if _, err := rdsepEmit(rdsepFixture(t, files)); err == nil || !strings.Contains(err.Error(), "Client.Odd") {
		t.Errorf("an endpoint with no method was not refused: %v", err)
	}
}

// On the real tree: the adapter's order path and its relay are marked, and
// wrappers the adapter never reaches are not.
func TestRdsep_RealTree(t *testing.T) {
	got, err := rdsepEmit(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `/setOrder` | POST | `Client.CreateOrder` | yes |",
		"| `/setOrder` | POST | `Client.CreateJoinOrder` | no |",
		"| `/orderDetails` | GET | `Client.GetOrderDetails` | yes |",
		"| `/generalRobokitAPI` | POST | `Client.GeneralRobokitAPI` | yes |",
		"| `/isFire` | GET | `Client.GetFireAlarmStatus` | yes |",
		"| `/licInfo` | GET | `Client.GetLicenseInfo` | no |",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s", w)
		}
	}
}
