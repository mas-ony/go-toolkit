package config

// Tests for notification_telegram_config.go.
//
// What the section promises: a token in the shape BotFather issues,
// required and never quoted; an API URL that is either empty, for the
// hosted API, or an http(s) URL without userinfo, query or fragment, with
// plain http allowed to loopback alone; a timeout of at least a second;
// and a log line that says whether the token is set without printing it.

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// testBotToken has the shape BotFather issues and is worth nothing.
const testBotToken = "123456789:AAFakeTokenForTests_0123456789abcdefg"

// telegramBase is a configuration the telegram section accepts: the hosted
// API, with a token and a timeout. Each rule case changes it one key at a
// time.
var telegramBase = map[string]any{
	"notification.telegram.token":   testBotToken,
	"notification.telegram.timeout": "30s",
}

// buildTelegram adapts NewTelegramConfig to the constructor shape runRules
// takes.
func buildTelegram(v *viper.Viper) SectionConfig {
	return NewTelegramConfig(v)
}

// TestTelegramValidate holds the section's rules, each beside the value
// that satisfies it: the required token and its shape, the API URL forms
// and the one place plain http is allowed, and the timeout floor.
func TestTelegramValidate(t *testing.T) {
	t.Parallel()
	const token = "notification.telegram.token"
	const apiURL = "notification.telegram.api_url"
	const timeout = "notification.telegram.timeout"
	runRules(t, telegramBase, buildTelegram, []ruleCase{
		{"no token", map[string]any{token: nil},
			"notification.telegram.token is required"},
		{"a token with no secret", map[string]any{token: "123456789"},
			"is not a bot token"},
		{"a token with no id", map[string]any{token: ":AAFakeSecret"},
			"is not a bot token"},
		{"a token whose id is not a number",
			map[string]any{token: "bot123:AAFakeSecret"},
			"is not a bot token"},
		{"a token with a space inside",
			map[string]any{token: "123456789:AA Fake"},
			"is not a bot token"},
		// The token is put into a URL path, so a slash must not get there.
		{"a token with a slash",
			map[string]any{token: "123456789:AA/Fake"},
			"is not a bot token"},
		// A secret read from a file ends in a newline.
		{"a token padded with whitespace",
			map[string]any{token: "  " + testBotToken + "\n"}, ""},

		{"the hosted API spelled out",
			map[string]any{apiURL: "https://api.telegram.org"}, ""},
		{"a path prefix and a trailing slash",
			map[string]any{apiURL: "https://bot.example.go.id/telegram/"},
			""},
		{"a server of your own on loopback",
			map[string]any{apiURL: "http://127.0.0.1:8081"}, ""},
		{"a server on localhost",
			map[string]any{apiURL: "http://localhost:8081"}, ""},
		{"a server on the IPv6 loopback",
			map[string]any{apiURL: "http://[::1]:8081"}, ""},
		{"http to a remote host",
			map[string]any{apiURL: "http://bot.example.go.id"},
			"clear text"},
		// Loopback is matched exactly, as for notification.email.host.
		{"http to LOCALHOST",
			map[string]any{apiURL: "http://LOCALHOST:8081"}, "clear text"},
		{"no scheme",
			map[string]any{apiURL: "api.telegram.org"},
			"must start with http:// or https://"},
		{"another scheme",
			map[string]any{apiURL: "ftp://bot.example.go.id"},
			"must start with http:// or https://"},
		{"no host", map[string]any{apiURL: "https://"}, "names no host"},
		{"userinfo",
			map[string]any{apiURL: "https://bot:s3cret@bot.example.go.id"},
			"must not carry userinfo"},
		{"a query",
			map[string]any{apiURL: "https://bot.example.go.id/?a=1"},
			"query or fragment"},
		{"a fragment",
			map[string]any{apiURL: "https://bot.example.go.id/#x"},
			"query or fragment"},
		// An empty query still turns the appended path into something else.
		{"a bare question mark",
			map[string]any{apiURL: "https://bot.example.go.id?"},
			"query or fragment"},
		{"a value that does not parse",
			map[string]any{apiURL: "https://bot.example.go.id/%zz"},
			"not a valid URL"},

		{"no timeout", map[string]any{timeout: nil},
			"notification.telegram.timeout is required"},
		{"a bare-number timeout", map[string]any{timeout: 30},
			"at least 1s"},
		{"a negative timeout", map[string]any{timeout: "-5s"},
			"at least 1s"},
		{"a timeout of exactly a second", map[string]any{timeout: "1s"},
			""},
		{"async on", map[string]any{
			"notification.telegram.async": true}, ""},
	})
}

// NewTelegramConfig is documented as the authoritative list of the
// section's keys, so every one of them has to be read under the name the
// header comment gives it.
func TestNewTelegramConfigReadsEveryKey(t *testing.T) {
	t.Parallel()
	v := viper.New()
	for key, val := range map[string]any{
		"notification.telegram.token":   testBotToken,
		"notification.telegram.api_url": "https://bot.example.go.id/tg",
		"notification.telegram.timeout": "45s",
		"notification.telegram.async":   true,
	} {
		v.Set(key, val)
	}

	got := NewTelegramConfig(v)
	want := &TelegramConfig{
		Token:   testBotToken,
		APIURL:  "https://bot.example.go.id/tg",
		Timeout: 45 * time.Second,
		Async:   true,
	}
	if *got != *want {
		t.Errorf("NewTelegramConfig =\n%+v\nwant\n%+v", *got, *want)
	}
}

// Both strings lose stray whitespace on the way in, a trailing newline
// included, which is what a secret read from a file brings with it.
func TestNewTelegramConfigTrimsTokenAndURL(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("notification.telegram.token", " "+testBotToken+"\n")
	v.Set("notification.telegram.api_url", " https://bot.example.go.id/ ")

	got := NewTelegramConfig(v)
	if got.Token != testBotToken {
		t.Errorf("Token = %q, want it trimmed", got.Token)
	}
	if got.APIURL != "https://bot.example.go.id/" {
		t.Errorf("APIURL = %q, want it trimmed", got.APIURL)
	}
}

// An absent API URL stays empty rather than becoming the default, and a
// section nothing supplied is the zero value. The telegram package relies
// on the second to tell "not configured" from "configured halfway".
func TestTelegramAbsentValuesStayZero(t *testing.T) {
	t.Parallel()
	got := NewTelegramConfig(withKeys(telegramBase, nil))
	if got.APIURL != "" {
		t.Errorf("APIURL = %q, want it left empty", got.APIURL)
	}
	if *NewTelegramConfig(viper.New()) != (TelegramConfig{}) {
		t.Error("a section nothing supplied is not the zero value")
	}
}

// The address an empty API URL stands for is itself one Validate accepts.
func TestTelegramDefaultAPIURLIsValid(t *testing.T) {
	t.Parallel()
	c := &TelegramConfig{
		Token:   testBotToken,
		APIURL:  DefaultTelegramAPIURL,
		Timeout: time.Minute,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the default API URL is refused: %v", err)
	}
}

// A struct literal never passes through NewTelegramConfig, so Validate has
// to trim what it checks: a padded token is still a token.
func TestTelegramValidateNormalisesAStructLiteral(t *testing.T) {
	t.Parallel()
	c := &TelegramConfig{
		Token:   " " + testBotToken + "\n",
		APIURL:  " https://bot.example.go.id/ ",
		Timeout: 30 * time.Second,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("rejected: %v", err)
	}
}

// No message quotes what it refuses, because the error is printed to a
// terminal and pasted into tickets. The token is withheld whatever is
// wrong with it, and the URL is shown with its userinfo masked, including
// where the URL does not parse and the parser's own error quotes it.
func TestTelegramValidateNeverQuotesACredential(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3tvalue"
	for name, c := range map[string]*TelegramConfig{
		"a token with no colon": {
			Token: "not-a-token-" + secret, Timeout: time.Minute},
		"a token with a space": {
			Token: "123456789:" + secret + " x", Timeout: time.Minute},
		"userinfo in the URL": {Token: testBotToken, Timeout: time.Minute,
			APIURL: "https://bot:" + secret + "@bot.example.go.id"},
		"userinfo in a URL that does not parse": {
			Token: testBotToken, Timeout: time.Minute,
			APIURL: "https://bot:" + secret + "@bot.example.go.id/%zz"},
		"a slash inside the password": {
			Token: testBotToken, Timeout: time.Minute,
			APIURL: "https://bot:" + secret + "/x@bot.example.go.id"},
		"a space inside the password": {
			Token: testBotToken, Timeout: time.Minute,
			APIURL: "https://bot:" + secret + " x@bot.example.go.id"},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: validated", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error quotes the credential:\n%v", name, err)
		}
	}
}

// The token is reported as set or not and never printed, and the API URL
// shows the address a blank one stands for, with any userinfo masked.
func TestTelegramString(t *testing.T) {
	t.Parallel()
	with := (&TelegramConfig{Token: testBotToken, Async: true}).String()
	for _, want := range []string{
		"Token=(set)",
		"APIURL=https://api.telegram.org (default)",
		"Async=true",
	} {
		if !strings.Contains(with, want) {
			t.Errorf("String = %q, want it to show %s", with, want)
		}
	}
	for _, secret := range []string{testBotToken, "123456789", "AAFake"} {
		if strings.Contains(with, secret) {
			t.Errorf("String prints %q: %s", secret, with)
		}
	}

	if none := (&TelegramConfig{}).String(); !strings.Contains(none,
		"Token=(none)") {
		t.Errorf("String = %q, want Token=(none)", none)
	}

	masked := (&TelegramConfig{
		APIURL: "https://bot:s3cr3t@bot.example.go.id",
	}).String()
	if strings.Contains(masked, "s3cr3t") ||
		!strings.Contains(masked, "https://***@bot.example.go.id") {
		t.Errorf("String = %q, want the userinfo masked", masked)
	}
}
