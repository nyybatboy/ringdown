package twilio

// ringdown fork: the number's voice webhook, so an install can point its number at itself
// for inbound calls (dialout number).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Number is the subset of the IncomingPhoneNumber resource dialout uses.
type Number struct {
	SID         string `json:"sid"`
	PhoneNumber string `json:"phone_number"`
	VoiceURL    string `json:"voice_url"`
	VoiceMethod string `json:"voice_method"`
}

func (c *Client) numbersURL(sid string) string {
	u := c.base() + "/2010-04-01/Accounts/" + url.PathEscape(c.AccountSID) + "/IncomingPhoneNumbers"
	if sid != "" {
		u += "/" + url.PathEscape(sid)
	}
	return u + ".json"
}

// FindNumber returns the account's number with this E.164 value.
func (c *Client) FindNumber(ctx context.Context, e164 string) (*Number, error) {
	var page struct {
		Numbers []Number `json:"incoming_phone_numbers"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.numbersURL("")+"?PhoneNumber="+url.QueryEscape(e164), nil, &page); err != nil {
		return nil, err
	}
	if len(page.Numbers) == 0 {
		return nil, fmt.Errorf("twilio: %s isn't a number on this account", e164)
	}
	return &page.Numbers[0], nil
}

// SetVoiceURL points a number's incoming-call webhook at u (POST).
func (c *Client) SetVoiceURL(ctx context.Context, sid, u string) (*Number, error) {
	var n Number
	form := url.Values{"VoiceUrl": {u}, "VoiceMethod": {"POST"}}
	if err := c.doJSON(ctx, http.MethodPost, c.numbersURL(sid), form, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
