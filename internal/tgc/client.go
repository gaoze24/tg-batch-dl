// Package tgc wraps one long-running gotd client: connection lifecycle, login, chats, media listing and thumbnails.
package tgc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/util/tutil"
	"go.uber.org/zap"
	xproxy "golang.org/x/net/proxy"
)

// Telegram Desktop's public API credentials, the same pair `tdl login` uses; used unless the user configured their own
// app from my.telegram.org. Telegram's API terms ask third-party clients to use their own, hence the setting.
const (
	desktopAppID   = 2040
	desktopAppHash = "b18441a1ff607e10a989891a5462e627"
	poolSize       = 8
)

type State string

const (
	StateConnecting State = "connecting"
	StateLogin      State = "login"
	StateReady      State = "ready"
	StateOffline    State = "offline"
)

var ErrNotReady = errors.New("还没有连上或登录 Telegram")

type Self struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
}

type Status struct {
	State State      `json:"state"`
	Error string     `json:"error,omitempty"`
	Self  *Self      `json:"self,omitempty"`
	Login *LoginView `json:"login,omitempty"`
}

type Client struct {
	log         *zap.Logger
	sessionPath string
	proxy       string
	appID       int
	appHash     string

	mu          sync.RWMutex
	state       State
	lastErr     string
	client      *telegram.Client
	api         *tg.Client
	pool        dcpool.Pool
	disp        tg.UpdateDispatcher
	runCtx      context.Context
	self        *Self
	restart     context.CancelFunc
	wipeSession bool
	login       *loginFlow

	chats  chatCache
	thumbs *thumbCache
}

// New prepares a client; appID 0 means Telegram Desktop's public credentials.
func New(sessionPath, proxy string, appID int, appHash string, log *zap.Logger) *Client {
	if appID == 0 || appHash == "" {
		appID, appHash = desktopAppID, desktopAppHash
	}
	return &Client{
		log:         log,
		sessionPath: sessionPath,
		proxy:       proxy,
		appID:       appID,
		appHash:     appHash,
		state:       StateConnecting,
		thumbs:      newThumbCache(),
	}
}

// Run keeps a connection alive until ctx is done, reconnecting after failures.
func (c *Client) Run(ctx context.Context) {
	for ctx.Err() == nil {
		runCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.restart = cancel
		c.mu.Unlock()

		err := c.runOnce(runCtx)
		cancel()

		c.mu.Lock()
		wipe := c.wipeSession
		c.wipeSession = false
		c.mu.Unlock()
		if wipe {
			if rmErr := os.Remove(c.sessionPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				c.log.Warn("remove session", zap.Error(rmErr))
			}
		}

		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			c.log.Warn("telegram connection ended", zap.Error(err))
			c.setState(StateOffline, "连接 Telegram 失败："+err.Error()+"（5 秒后重试）")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	c.setState(StateConnecting, "")
	dial, err := dialer(c.proxy)
	if err != nil {
		return err
	}
	disp := tg.NewUpdateDispatcher()
	client := telegram.NewClient(c.appID, c.appHash, telegram.Options{
		Resolver:       dcs.Plain(dcs.PlainOptions{Dial: dial}),
		SessionStorage: &session.FileStorage{Path: c.sessionPath},
		UpdateHandler:  disp,
		Device:         tutil.Device,
		Middlewares:    []telegram.Middleware{floodWait(c.log)},
		RetryInterval:  5 * time.Second,
		MaxRetries:     5,
		DialTimeout:    15 * time.Second,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		pool := dcpool.NewPool(client, poolSize, floodWait(c.log))
		defer func() { _ = pool.Close() }()

		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}

		c.mu.Lock()
		c.client, c.api, c.pool, c.disp, c.runCtx = client, client.API(), pool, disp, ctx
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.cancelLoginLocked()
			c.client, c.api, c.pool, c.runCtx = nil, nil, nil, nil
			c.mu.Unlock()
		}()

		if status.Authorized {
			c.setReady(status.User)
		} else {
			c.setState(StateLogin, "")
		}
		<-ctx.Done()
		return ctx.Err()
	})
}

func dialer(proxy string) (dcs.DialFunc, error) {
	if proxy == "" {
		var d net.Dialer
		return d.DialContext, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("代理地址格式错误: %w", err)
	}
	pd, err := xproxy.FromURL(u, xproxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("代理地址不可用: %w", err)
	}
	cd, ok := pd.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("代理不支持超时控制")
	}
	return cd.DialContext, nil
}

func (c *Client) setState(s State, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = s
	c.lastErr = errMsg
	if s != StateReady {
		c.self = nil
	}
}

func (c *Client) setReady(u *tg.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateReady
	c.lastErr = ""
	c.self = selfFrom(u)
}

func selfFrom(u *tg.User) *Self {
	if u == nil {
		return nil
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = u.Username
	}
	return &Self{ID: u.ID, Name: name, Username: u.Username}
}

// Status is a snapshot for the web UI.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := Status{State: c.state, Error: c.lastErr}
	if c.self != nil {
		s := *c.self
		st.Self = &s
	}
	if c.login != nil {
		v := c.login.view
		st.Login = &v
	}
	return st
}

// Session returns the raw API and the DC pool while logged in.
func (c *Client) Session() (*tg.Client, dcpool.Pool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != StateReady || c.api == nil || c.pool == nil {
		return nil, nil, ErrNotReady
	}
	return c.api, c.pool, nil
}

// Logout signs this session out on Telegram's side and deletes the local session file.
func (c *Client) Logout(ctx context.Context) error {
	api, _, err := c.Session()
	if err != nil {
		return err
	}
	if _, err := api.AuthLogOut(ctx); err != nil {
		c.log.Warn("auth.logOut", zap.Error(err)) // still wipe locally
	}
	c.mu.Lock()
	c.wipeSession = true
	restart := c.restart
	c.mu.Unlock()
	c.chats.reset()
	c.thumbs.reset()
	if restart != nil {
		restart()
	}
	return nil
}
