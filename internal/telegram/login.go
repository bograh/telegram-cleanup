package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"strings"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"rsc.io/qr"
)

// CodeInfo describes where Telegram delivered a login code, so the UI can
// tell the user where to look instead of guessing.
type CodeInfo struct {
	Where     string // human description of the delivery channel
	Next      string // what "Resend" will switch to, if anything
	CanResend bool
	// Blocked is set when Telegram won't send a code at all (e.g. it requires
	// a login email to be set up first). QR login still works then.
	Blocked bool
}

func describeSent(t tg.AuthSentCodeTypeClass) (where string, blocked bool) {
	switch v := t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return "in your Telegram app: open the official “Telegram” chat (blue check mark) on a device where you're already logged in", false
	case *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase, *tg.AuthSentCodeTypeFirebaseSMS:
		return "by SMS to your phone", false
	case *tg.AuthSentCodeTypeCall:
		return "by a phone call that reads out the code", false
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return "as a missed call: the code is the last digits of the calling number", false
	case *tg.AuthSentCodeTypeEmailCode:
		return fmt.Sprintf("by email to %s (your Telegram login email)", v.EmailPattern), false
	case *tg.AuthSentCodeTypeFragmentSMS:
		return "via Fragment (anonymous number) at " + v.URL, false
	case *tg.AuthSentCodeTypeSetUpEmailRequired:
		return "nowhere yet: Telegram requires this account to set up a login email before signing in to a new app", true
	}
	return fmt.Sprintf("by %T", t), false
}

func describeNext(t tg.AuthCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthCodeTypeSMS:
		return "SMS"
	case *tg.AuthCodeTypeCall:
		return "a phone call"
	case *tg.AuthCodeTypeFlashCall, *tg.AuthCodeTypeMissedCall:
		return "a missed call"
	case *tg.AuthCodeTypeFragmentSMS:
		return "Fragment SMS"
	}
	return ""
}

// handleSent stores the code hash and describes the delivery.
func (c *Client) handleSent(phone string, sent tg.AuthSentCodeClass) (CodeInfo, error) {
	switch v := sent.(type) {
	case *tg.AuthSentCode:
		where, blocked := describeSent(v.Type)
		info := CodeInfo{Where: where, Blocked: blocked}
		if next, ok := v.GetNextType(); ok {
			info.Next = describeNext(next)
			info.CanResend = true
		}
		log.Printf("login code sent %s (type %T, next %q, timeout %ds)", where, v.Type, info.Next, v.Timeout)
		c.mu.Lock()
		c.phone, c.codeHash = phone, v.PhoneCodeHash
		c.mu.Unlock()
		return info, nil
	case *tg.AuthSentCodeSuccess:
		// Telegram recognised this device and logged in without a code.
		a, _ := v.Authorization.(*tg.AuthAuthorization)
		if a != nil {
			u, _ := a.User.(*tg.User)
			c.setAuthed(u)
		}
		return CodeInfo{}, ErrAlreadyAuthorized
	}
	return CodeInfo{}, fmt.Errorf("unexpected send-code response %T", sent)
}

// ErrAlreadyAuthorized is returned when sending a code logged us in directly.
var ErrAlreadyAuthorized = errors.New("already logged in")

// SendCode asks Telegram to send a login code and reports where it went.
func (c *Client) SendCode(ctx context.Context, phone string) (CodeInfo, error) {
	if err := c.wait(ctx); err != nil {
		return CodeInfo{}, err
	}
	sent, err := c.tc.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return CodeInfo{}, err
	}
	return c.handleSent(phone, sent)
}

// ResendCode asks Telegram to deliver the code again, usually through the
// next channel (SMS or call).
func (c *Client) ResendCode(ctx context.Context) (CodeInfo, error) {
	if err := c.wait(ctx); err != nil {
		return CodeInfo{}, err
	}
	c.mu.Lock()
	phone, hash := c.phone, c.codeHash
	c.mu.Unlock()
	if hash == "" {
		return CodeInfo{}, errors.New("request a code first")
	}
	sent, err := c.tc.Auth().ResendCode(ctx, phone, hash)
	if err != nil {
		return CodeInfo{}, err
	}
	return c.handleSent(phone, sent)
}

// SignIn completes login with the code. Returns ErrPasswordNeeded for 2FA accounts.
func (c *Client) SignIn(ctx context.Context, code string) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	phone, hash := c.phone, c.codeHash
	c.mu.Unlock()
	if hash == "" {
		return errors.New("request a code first")
	}
	a, err := c.tc.Auth().SignIn(ctx, phone, strings.TrimSpace(code), hash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		return ErrPasswordNeeded
	}
	if err != nil {
		return err
	}
	u, _ := a.User.(*tg.User)
	c.setAuthed(u)
	return nil
}

// QRState is what the login page should show for QR login.
type QRState struct {
	Image   string // PNG data URI of the current QR code
	Done    bool   // logged in
	Expires time.Time
}

// QR returns the current QR login state. Call it repeatedly (the page polls
// every couple of seconds): it re-exports the token when it expires and
// completes the login once the phone has accepted it. Telegram is only
// contacted when the token expires or the accept signal arrives.
func (c *Client) QR(ctx context.Context) (QRState, error) {
	if err := c.wait(ctx); err != nil {
		return QRState{}, err
	}
	q := c.tc.QR()

	select {
	case <-c.loggedIn:
		a, err := q.Import(ctx)
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			return QRState{}, ErrPasswordNeeded
		}
		if err != nil {
			return QRState{}, err
		}
		u, _ := a.User.(*tg.User)
		c.setAuthed(u)
		return QRState{Done: true}, nil
	default:
	}

	c.mu.Lock()
	tok := c.qrToken
	c.mu.Unlock()
	if tok.Empty() || time.Until(tok.Expires()) < 3*time.Second {
		res, err := q.ExportResult(ctx)
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			return QRState{}, ErrPasswordNeeded
		}
		if err != nil {
			return QRState{}, err
		}
		switch {
		case res.Authorization != nil:
			u, _ := res.Authorization.User.(*tg.User)
			c.setAuthed(u)
			return QRState{Done: true}, nil
		case res.MigrateTo != nil:
			a, err := q.Import(ctx)
			if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
				return QRState{}, ErrPasswordNeeded
			}
			if err != nil {
				return QRState{}, err
			}
			u, _ := a.User.(*tg.User)
			c.setAuthed(u)
			return QRState{Done: true}, nil
		}
		tok = res.Token
		c.mu.Lock()
		c.qrToken = tok
		c.mu.Unlock()
	}

	code, err := qr.Encode(tok.URL(), qr.M)
	if err != nil {
		return QRState{}, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, qrImage(code, 8)); err != nil {
		return QRState{}, err
	}
	return QRState{
		Image:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		Expires: tok.Expires(),
	}, nil
}

// qrImage renders the code with `scale` pixels per module and the standard
// 4-module quiet zone. (rsc.io/qr's own Image() reports a scaled size but
// draws unscaled, which leaves a tiny code in a corner.)
func qrImage(c *qr.Code, scale int) image.Image {
	const quiet = 4
	size := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, size, size))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Black(x, y) {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray((x+quiet)*scale+dx, (y+quiet)*scale+dy, color.Gray{})
				}
			}
		}
	}
	return img
}
