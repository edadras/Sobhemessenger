package ratelimit

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sobh/messenger/backend/internal/config"
)

// Rules is the concrete limit set, built from configuration so operators can
// tighten limits without a redeploy.
type Rules struct {
	OTPPerPhone     Rule
	OTPPerIP        Rule
	LoginPerIP      Rule
	APIPerUser      Rule
	APIPerIP        Rule
	MessagesPerUser Rule
	UploadsPerUser  Rule
	SearchPerUser   Rule
	ContactSync     Rule
}

func NewRules(cfg config.RateLimits) Rules {
	return Rules{
		// Authentication limits fail closed: if Redis is down we would rather
		// reject a login than leave OTP brute-forcing unbounded (§33).
		OTPPerPhone: Rule{
			Name: "otp_phone", Limit: cfg.OTPPerPhonePerHour, Window: time.Hour, FailClosed: true,
		},
		OTPPerIP: Rule{
			Name: "otp_ip", Limit: cfg.OTPPerIPPerHour, Window: time.Hour, FailClosed: true,
		},
		LoginPerIP: Rule{
			Name: "login_ip", Limit: cfg.LoginPerIPPerHour, Window: time.Hour, FailClosed: true,
		},
		APIPerUser: Rule{
			Name: "api_user", Limit: cfg.APIPerUserPerMin, Window: time.Minute,
		},
		APIPerIP: Rule{
			Name: "api_ip", Limit: cfg.APIPerIPPerMin, Window: time.Minute,
		},
		MessagesPerUser: Rule{
			Name: "messages_user", Limit: cfg.MessagesPerMin, Window: time.Minute,
		},
		UploadsPerUser: Rule{
			Name: "uploads_user", Limit: cfg.UploadsPerHour, Window: time.Hour,
		},
		SearchPerUser: Rule{
			Name: "search_user", Limit: 120, Window: time.Minute,
		},
		ContactSync: Rule{
			Name: "contact_sync", Limit: 10, Window: time.Hour,
		},
	}
}

// counter disambiguates two events recorded in the same microsecond. The
// sliding-window set is keyed by score+member, so members must be unique.
var counter atomic.Uint64

func randomSuffix() string {
	return strconv.FormatUint(counter.Add(1), 36)
}
