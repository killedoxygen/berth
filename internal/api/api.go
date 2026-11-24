// Package api is the daemon protocol: one JSON object per line over a Unix
// socket. Requests may carry file descriptors (SCM_RIGHTS) with their first
// line; streaming operations answer with several lines.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
)

const Version = 1

type Request struct {
	Op      string    `json:"op"`
	ID      string    `json:"id,omitempty"`
	Spec    *job.Spec `json:"spec,omitempty"`
	Bind    bool      `json:"bind,omitempty"`     // cancel the job when the bound process exits
	BindPID int       `json:"bind_pid,omitempty"` // process to bind; default: the connecting process
	Watch   bool      `json:"watch,omitempty"`    // submit: stream events until the job ends
	Token   string    `json:"token,omitempty"`
	Offset  int64     `json:"offset,omitempty"`
	Limit   int       `json:"limit,omitempty"`
	Follow  bool      `json:"follow,omitempty"`
	All     bool      `json:"all,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Update  *Update   `json:"update,omitempty"`
}

type Update struct {
	Kind     *job.Kind `json:"kind,omitempty"`
	Priority *int      `json:"priority,omitempty"`
	Memory   *int64    `json:"memory,omitempty"`
	CPUs     *float64  `json:"cpus,omitempty"`
	Retries  *int      `json:"retries,omitempty"`
}

// Error codes are stable for programs.
const (
	ENotFound    = "not_found"
	EInvalid     = "invalid"
	EDenied      = "denied"
	EUnavailable = "unavailable"
	EInternal    = "internal"
)

type Response struct {
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Code    string    `json:"code,omitempty"`
	Job     *job.Job  `json:"job,omitempty"`
	Jobs    []job.Job `json:"jobs,omitempty"`
	Reused  bool      `json:"reused,omitempty"` // submit matched an active job with the same key
	Event   string    `json:"event,omitempty"`  // queued | started | exit
	Status  *Status   `json:"status,omitempty"`
	Data    string    `json:"data,omitempty"`
	Offset  int64     `json:"offset,omitempty"`
	Warning string    `json:"warning,omitempty"`
}

type Status struct {
	Version     string       `json:"version"`
	Mode        string       `json:"mode"` // user | system
	Backend     string       `json:"backend"`
	Admission   string       `json:"admission"` // "ready" or why nothing can start
	Health      probe.Health `json:"health"`
	Pool        PoolUsage    `json:"pool"`
	Maintenance *Maintenance `json:"maintenance,omitempty"`
	Jobs        []job.Job    `json:"jobs"`
}

type PoolUsage struct {
	Memory         int64          `json:"memory"`
	MemoryCharged  int64          `json:"memory_charged"`
	Overhead       int64          `json:"overhead"`
	CPUs           float64        `json:"cpus"`
	CPUsCharged    float64        `json:"cpus_charged"`
	Resources      map[string]int `json:"resources,omitempty"`
	ResourcesInUse map[string]int `json:"resources_in_use,omitempty"`
	DefaultMemory  int64          `json:"default_memory"`
	DefaultCPUs    float64        `json:"default_cpus"`
	FitsNowMemory  int64          `json:"fits_now_memory"` // largest --memory that would start immediately
	FitsNowCPUs    float64        `json:"fits_now_cpus"`
}

type Maintenance struct {
	Running  bool      `json:"running"`
	Finished time.Time `json:"finished,omitempty"`
	Summary  string    `json:"summary,omitempty"`
}

// Conn is one connection, either side.
type Conn struct {
	c   *net.UnixConn
	buf []byte
	fds []int
}

func Wrap(c *net.UnixConn) *Conn { return &Conn{c: c} }

func Dial(socket string) (*Conn, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	return &Conn{c: c}, nil
}

func (c *Conn) Close() error       { return c.c.Close() }
func (c *Conn) Raw() *net.UnixConn { return c.c }

// Send writes one message, optionally carrying descriptors.
func (c *Conn) Send(v any, fds ...int) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(fds) == 0 {
		_, err = c.c.Write(data)
		return err
	}
	n, oobn, err := c.c.WriteMsgUnix(data, syscall.UnixRights(fds...), nil)
	if err != nil {
		return err
	}
	if n < len(data) || oobn == 0 {
		if n < len(data) {
			_, err = c.c.Write(data[n:])
		}
		if oobn == 0 && err == nil {
			err = errors.New("descriptors were not sent")
		}
	}
	return err
}

// Recv reads one message into v. Descriptors that arrived are kept; take them with TakeFDs.
func (c *Conn) Recv(v any) error {
	for {
		if i := bytes.IndexByte(c.buf, '\n'); i >= 0 {
			line := c.buf[:i]
			c.buf = c.buf[i+1:]
			return json.Unmarshal(line, v)
		}
		if len(c.buf) > 1<<20 {
			return errors.New("message too large")
		}
		chunk := make([]byte, 64<<10)
		oob := make([]byte, syscall.CmsgSpace(16*4))
		n, oobn, _, _, err := c.c.ReadMsgUnix(chunk, oob)
		if oobn > 0 {
			msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
			if perr == nil {
				for _, m := range msgs {
					if fds, err := syscall.ParseUnixRights(&m); err == nil {
						c.fds = append(c.fds, fds...)
					}
				}
			}
		}
		if n > 0 {
			c.buf = append(c.buf, chunk[:n]...)
			continue
		}
		if err != nil {
			return err
		}
		return errors.New("connection closed")
	}
}

// TakeFDs returns descriptors received so far; the caller owns them.
func (c *Conn) TakeFDs() []int {
	fds := c.fds
	c.fds = nil
	return fds
}

// SetDeadline bounds the next reads and writes.
func (c *Conn) SetDeadline(t time.Time) error { return c.c.SetDeadline(t) }

// Call sends one request and reads one response, turning daemon errors into Go errors.
func Call(socket string, req Request) (*Response, error) {
	c, err := Dial(socket)
	if err != nil {
		return nil, Unavailable(socket, err)
	}
	defer c.Close()
	if err := c.Send(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := c.Recv(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return &resp, &Error{Code: resp.Code, Message: resp.Error}
	}
	return &resp, nil
}

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Unavailable explains a failed connection with the fix.
func Unavailable(socket string, err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return &Error{Code: EUnavailable, Message: fmt.Sprintf("berth daemon is not running (%s); start it with `berth setup` (installs a service) or `berth daemon`", socket)}
	}
	return &Error{Code: EUnavailable, Message: fmt.Sprintf("cannot reach berth daemon at %s: %v", socket, err)}
}
