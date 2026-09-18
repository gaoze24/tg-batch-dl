package tgc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
	"rsc.io/qr"
)

// LoginView is what the web UI needs to render the current login step.
type LoginView struct {
	Method       string `json:"method"` // qr | phone
	Step         string `json:"step"`   // starting | qr | code | password | error
	Error        string `json:"error,omitempty"`
	QRVersion    int64  `json:"qr_version,omitempty"` // bumps whenever a new QR token is issued
	PasswordHint string `json:"password_hint,omitempty"`
}

type loginFlow struct {
	view   LoginView
	token  qrlogin.Token
	codeCh chan string
	pwdCh  chan string
	cancel context.CancelFunc
}

var ErrNoLogin = errors.New("当前没有进行中的登录")

// begin replaces any running login flow; the caller starts the goroutine.
func (c *Client) begin(method string) (*loginFlow, context.Context, *telegram.Client, tg.UpdateDispatcher, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateLogin || c.client == nil || c.runCtx == nil {
		return nil, nil, nil, tg.UpdateDispatcher{}, errors.New("当前不需要登录")
	}
	c.cancelLoginLocked()
	ctx, cancel := context.WithCancel(c.runCtx)
	flow := &loginFlow{
		view:   LoginView{Method: method, Step: "starting"},
		codeCh: make(chan string, 1),
		pwdCh:  make(chan string, 1),
		cancel: cancel,
	}
	c.login = flow
	return flow, ctx, c.client, c.disp, nil
}

func (c *Client) cancelLoginLocked() {
	if c.login != nil {
		c.login.cancel()
		c.login = nil
	}
}

// CancelLogin abandons the current login attempt.
func (c *Client) CancelLogin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelLoginLocked()
}

func (c *Client) update(flow *loginFlow, f func(*loginFlow)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.login == flow {
		f(flow)
	}
}

func (c *Client) fail(flow *loginFlow, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	c.log.Warn("login failed", zap.Error(err))
	c.update(flow, func(f *loginFlow) {
		f.view.Step = "error"
		f.view.Error = explain(err)
	})
}

func (c *Client) finish(ctx context.Context, flow *loginFlow, client *telegram.Client) {
	user, err := client.Self(ctx)
	if err != nil {
		c.fail(flow, err)
		return
	}
	c.mu.Lock()
	if c.login == flow {
		c.login = nil
	}
	c.mu.Unlock()
	flow.cancel()
	c.chats.reset()
	c.setReady(user)
}

// StartQR shows a QR code that the phone app scans (Settings → Devices → Link Desktop Device).
func (c *Client) StartQR() error {
	flow, ctx, client, disp, err := c.begin("qr")
	if err != nil {
		return err
	}
	go func() {
		loggedIn := qrlogin.OnLoginToken(disp)
		_, err := client.QR().Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
			c.update(flow, func(f *loginFlow) {
				f.token = token
				f.view.Step = "qr"
				f.view.QRVersion = time.Now().UnixNano()
			})
			return nil
		})
		switch {
		case err == nil:
			c.finish(ctx, flow, client)
		case tgerr.Is(err, "SESSION_PASSWORD_NEEDED"), errors.Is(err, auth.ErrPasswordAuthNeeded):
			c.passwordLoop(ctx, flow, client)
		default:
			c.fail(flow, err)
		}
	}()
	return nil
}

// QRImage renders the current QR token as PNG.
func (c *Client) QRImage() ([]byte, error) {
	c.mu.RLock()
	flow := c.login
	var token qrlogin.Token
	ok := flow != nil && flow.view.Step == "qr"
	if ok {
		token = flow.token
	}
	c.mu.RUnlock()
	if !ok {
		return nil, ErrNoLogin
	}
	img, err := token.Image(qr.M)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// StartPhone sends a login code to the given phone number (international format, e.g. +65...).
func (c *Client) StartPhone(phone string) error {
	phone = strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if len(phone) < 6 || !strings.HasPrefix(phone, "+") {
		return errors.New("手机号要带国家区号，例如 +6581234567")
	}
	flow, ctx, client, _, err := c.begin("phone")
	if err != nil {
		return err
	}
	go func() {
		sent, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
		if err != nil {
			c.fail(flow, err)
			return
		}
		switch s := sent.(type) {
		case *tg.AuthSentCodeSuccess:
			c.finish(ctx, flow, client)
		case *tg.AuthSentCode:
			c.codeLoop(ctx, flow, client, phone, s.PhoneCodeHash)
		default:
			c.fail(flow, fmt.Errorf("unexpected sendCode result %T", sent))
		}
	}()
	return nil
}

func (c *Client) codeLoop(ctx context.Context, flow *loginFlow, client *telegram.Client, phone, hash string) {
	c.update(flow, func(f *loginFlow) { f.view.Step = "code" })
	for {
		select {
		case <-ctx.Done():
			return
		case code := <-flow.codeCh:
			_, err := client.Auth().SignIn(ctx, phone, code, hash)
			var signUp *auth.SignUpRequired
			switch {
			case err == nil:
				c.finish(ctx, flow, client)
				return
			case errors.Is(err, auth.ErrPasswordAuthNeeded):
				c.passwordLoop(ctx, flow, client)
				return
			case tgerr.Is(err, "PHONE_CODE_INVALID"):
				c.update(flow, func(f *loginFlow) { f.view.Error = "验证码不对，请重新输入" })
			case errors.As(err, &signUp):
				c.fail(flow, errors.New("这个手机号还没有注册 Telegram"))
				return
			default:
				c.fail(flow, err)
				return
			}
		}
	}
}

func (c *Client) passwordLoop(ctx context.Context, flow *loginFlow, client *telegram.Client) {
	hint := ""
	if p, err := client.API().AccountGetPassword(ctx); err == nil {
		hint = p.Hint
	}
	c.update(flow, func(f *loginFlow) {
		f.view.Step = "password"
		f.view.PasswordHint = hint
		f.view.Error = ""
	})
	for {
		select {
		case <-ctx.Done():
			return
		case pwd := <-flow.pwdCh:
			_, err := client.Auth().Password(ctx, pwd)
			switch {
			case err == nil:
				c.finish(ctx, flow, client)
				return
			case errors.Is(err, auth.ErrPasswordInvalid):
				c.update(flow, func(f *loginFlow) { f.view.Error = "两步验证密码不对，请重试" })
			default:
				c.fail(flow, err)
				return
			}
		}
	}
}

func (c *Client) submit(step string, value string, pick func(*loginFlow) chan string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	flow := c.login
	if flow == nil || flow.view.Step != step {
		return ErrNoLogin
	}
	flow.view.Error = ""
	select {
	case pick(flow) <- value:
		return nil
	default:
		return errors.New("正在验证上一次输入，请稍等")
	}
}

func (c *Client) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("请输入验证码")
	}
	return c.submit("code", code, func(f *loginFlow) chan string { return f.codeCh })
}

func (c *Client) SubmitPassword(pwd string) error {
	if pwd == "" {
		return errors.New("请输入两步验证密码")
	}
	return c.submit("password", pwd, func(f *loginFlow) chan string { return f.pwdCh })
}

// explain turns common Telegram errors into something a user can act on.
func explain(err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("操作太频繁，请 %d 秒后再试", int(d.Seconds()))
	}
	if e, ok := tgerr.As(err); ok {
		switch e.Type {
		case "PHONE_NUMBER_INVALID":
			return "手机号格式不对，要带国家区号，例如 +6581234567"
		case "PHONE_NUMBER_BANNED":
			return "这个手机号被 Telegram 封禁了"
		case "PHONE_CODE_EXPIRED":
			return "验证码过期了，请重新开始"
		case "AUTH_TOKEN_EXPIRED", "AUTH_TOKEN_INVALID":
			return "二维码过期了，请重新生成"
		case "PASSWORD_HASH_INVALID":
			return "两步验证密码不对"
		}
	}
	return err.Error()
}
