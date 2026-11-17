// Package smbclient opens SMB2/3 shares without kernel mounts or helper programs.
package smbclient

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

const (
	ConnectTimeout = 3 * time.Second
	IOTimeout      = 30 * time.Second
)

// Parse rejects credentials in URLs: they belong in the separately saved fields.
func Parse(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "smb" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("smb: expected smb://host[:port]/share[/folder], without credentials")
	}
	if _, err := Relative(strings.TrimPrefix(u.Path, "/")); err != nil || strings.Trim(u.Path, "/") == "" {
		return nil, errors.New("smb: invalid share path")
	}
	return u, nil
}

// Relative keeps remote paths inside the configured share, including on Windows servers.
func Relative(p string) (string, error) {
	if strings.ContainsAny(p, "\\\x00:") || strings.HasPrefix(p, "/") {
		return "", errors.New("smb: invalid relative path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("smb: path leaves the configured folder")
		}
	}
	return path.Clean(p), nil
}

type Client struct {
	Share   *smb2.Share
	Session *smb2.Session
	Root    string
	conn    net.Conn
	stop    func() bool
}

// Dial bounds negotiation, login and every subsequent socket read/write. Closing
// the connection on cancellation also stops the library's background receiver.
func Dial(ctx context.Context, raw, user, password string, dial func(context.Context, string, string) (net.Conn, error)) (*Client, error) {
	u, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "445"
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: ConnectTimeout}).DialContext
	}
	connect, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	conn, err := dial(connect, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	c.stop = context.AfterFunc(ctx, func() { conn.Close() })
	d := smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: password}}
	session, err := d.DialContext(connect, deadlineConn{conn})
	if err == nil {
		c.Session = session.WithContext(ctx)
		share, root, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		c.Root = root
		c.Share, err = session.WithContext(connect).Mount(share)
		if err == nil {
			c.Share = c.Share.WithContext(ctx)
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error {
	c.stop()
	return c.conn.Close()
}

type deadlineConn struct{ net.Conn }

func (c deadlineConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(IOTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c deadlineConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(IOTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}
