// Package redacttest holds synthetic planted secrets for redaction tests.
package redacttest

import (
	"encoding/json"
	"strings"
)

// Planted secrets are assembled at run time so no literal token shape
// sits in the repository (secret scanners and push protection would flag
// it). None of them is a real credential.

func rep(s string, n int) string { return strings.Repeat(s, n)[:n] }

// base62 is a deterministic pseudo-random run of n base62 characters.
func base62(seed, n int) string {
	const a = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	x := uint32(seed*2654435761 + 12345)
	for range n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b.WriteByte(a[x%62])
	}
	return b.String()
}

func hexs(seed, n int) string {
	return strings.ToLower(strings.Map(func(r rune) rune { return rune("0123456789abcdef"[int(r)%16]) }, base62(seed, n)))
}

// Planted maps a placeholder name to a synthetic secret and the rule
// that must catch it.
type Planted struct {
	Name, Value, Rule string
}

// PlantedSecrets is the fixture set shared by this package's tests and
// the harness-format tests elsewhere (see Fill).
func PlantedSecrets() []Planted {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + base62(40, 64) + "\n" + base62(41, 64) + "\n" + base62(42, 20) + "==\n-----END RSA PRIVATE KEY-----"
	return []Planted{
		{"AWS_KEY", "AK" + "IA" + strings.ToUpper(rep("Q2R3S4T5U6V7W2X3", 16)), "aws-access-key"},
		{"GITHUB_PAT", "gh" + "p_" + base62(1, 36), "github-token"},
		{"GITHUB_FINE", "github" + "_pat_" + base62(2, 82), "github-fine-grained-pat"},
		{"GITLAB", "gl" + "pat-" + base62(3, 20), "gitlab-token"},
		{"ANTHROPIC", "sk-" + "ant-api03-" + base62(4, 93) + "AA", "anthropic-key"},
		{"OPENAI", "sk-" + "proj-" + base62(5, 48), "openai-key"},
		{"SLACK", "xo" + "xb-" + "1234567890-" + base62(6, 24), "slack-token"},
		{"STRIPE", "sk" + "_live_" + base62(7, 24), "stripe-key"},
		{"GOOGLE", "AI" + "za" + base62(8, 35), "google-api-key"},
		{"NPM", "np" + "m_" + base62(9, 36), "npm-token"},
		{"HF", "h" + "f_" + strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return 'q'
			}
			return r
		}, base62(10, 34)), "huggingface-token"},
		{"TAILSCALE", "ts" + "key-auth-" + base62(11, 12) + "-" + base62(12, 30), "tailscale-key"},
		{"FLOPWIRE", "flop" + "wire_" + base62(13, 43), "flopwire-token"},
		{"JWT", "ey" + "J" + base62(14, 30) + ".ey" + "J" + base62(15, 40) + "." + base62(16, 43), "jwt"},
		{"PEM", pem, "private-key"},
		{"DB_URL", "postgres://app:" + "Pw9x" + base62(17, 12) + "@db.internal:5432/app", "url-password"},
		{"ENV_SECRET", "MY_SERVICE_" + "TOKEN=" + base62(18, 32), "assignment"},
	}
}

// Fill replaces every {{NAME}} in tmpl with the planted secret of that
// name, JSON-string-escaped depth times: 0 for plain text, 1 inside a
// JSON string, 2 inside JSON that is itself a string value (a Devin
// chat_message, Codex function_call arguments). {{NAME@k}} overrides the
// depth for one placeholder.
func Fill(tmpl string, depth int) string {
	for _, p := range PlantedSecrets() {
		for k := range 4 {
			tmpl = strings.ReplaceAll(tmpl, "{{"+p.Name+"@"+string(rune('0'+k))+"}}", escape(p.Value, k))
		}
		tmpl = strings.ReplaceAll(tmpl, "{{"+p.Name+"}}", escape(p.Value, depth))
	}
	return tmpl
}

func escape(v string, depth int) string {
	for range depth {
		b, _ := json.Marshal(v)
		v = string(b[1 : len(b)-1])
	}
	return v
}

// Needles are the substrings that must not survive redaction: each whole
// token, or for rules that mask only part of a match (URL password, PEM
// body, assignment value) that part.
func Needles() map[string]string {
	out := map[string]string{}
	for _, p := range PlantedSecrets() {
		v := p.Value
		switch p.Name {
		case "DB_URL":
			v = v[strings.Index(v, ":"+"Pw9x")+1 : strings.Index(v, "@")]
		case "PEM":
			v = base62(41, 64)
		case "ENV_SECRET":
			v = v[strings.Index(v, "=")+1:]
		}
		out[p.Name] = v
	}
	return out
}
