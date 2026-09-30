// Secret detection rules.
//
// The vendor token shapes follow gitleaks config/gitleaks.toml
// (https://github.com/gitleaks/gitleaks, commit
// b58d3f102cf3a2c84cb7f923d05c25c9b1aed84b, MIT, see
// third_party/gitleaks), rule ids aws-access-token, github-*, gitlab-pat,
// anthropic-*, openai-api-key, slack-*, stripe-access-token, gcp-api-key,
// npm-access-token, pypi-upload-token, huggingface-access-token,
// sendgrid-api-token, digitalocean-*, databricks-api-token,
// linear-api-key, age-secret-key, private-key and jwt. The regexes were
// rewritten for this package (leading boundaries are checked in code so a
// JSON escape such as \n before a token still counts as a boundary), and
// the keyword-assignment rule is Flopwire's own procedural matcher, not
// gitleaks' generic-api-key.

package redact

import (
	"bytes"
	"regexp"
	"strings"
)

// RulesVersion names the rule set. Bump it whenever a rule is added,
// removed or changed: the device sends it with every flush, and the
// server records which rules redacted each generation.
const RulesVersion = "flopwire-redact/1"

// rule is one regex detector. The regex runs only on a segment that
// contains one of its keywords (case-sensitive), so rare anchors keep the
// scan cheap.
type rule struct {
	id       string
	keywords []string
	re       *regexp.Regexp
	group    int  // capture group to mask; 0 is the whole match
	hashed   bool // the marker carries a short hash (high-entropy tokens)
	// bound: the match must not continue an alphanumeric run on the left
	// or right (a JSON escape like \n counts as a boundary on the left).
	bound    bool
	validate func([]byte) bool
	// skip, given the regex's submatches, drops a match by its context.
	skip func(sub [][]byte) bool
	// The regex runs on [hit-back, hit+fwd) around each keyword hit (hit
	// is where the keyword starts), widened while a match touches the
	// window's end. back covers what the regex can match before its
	// keyword.
	back, fwd int
	// ci: keywords match in any case (otherwise exactly as written).
	ci bool
}

// tok is a vendor token rule: hashed and bounded.
func tok(id string, keywords []string, re string) rule {
	return rule{id: id, keywords: keywords, re: regexp.MustCompile(re), hashed: true, bound: true}
}

var rules = []rule{
	tok("private-key", []string{"PRIVATE KEY"}, ``), // replaced in init: needs a group and a validator
	tok("aws-access-key", []string{"AKIA", "ASIA", "ABIA", "ACCA", "A3T"}, `(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}`),
	tok("github-token", []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, `gh[pousr]_[0-9A-Za-z]{36}`),
	tok("github-fine-grained-pat", []string{"github_pat_"}, `github_pat_[0-9A-Za-z_]{82}`),
	tok("gitlab-token", []string{"glpat-", "gloas-", "gldt-", "glrt-"}, `gl(?:pat|oas|dt|rt)-[0-9A-Za-z_\-]{20,}`),
	tok("anthropic-key", []string{"sk-ant-"}, `sk-ant-[a-z]{2,8}[0-9]{2}-[0-9A-Za-z_\-]{20,}`),
	tok("openai-key", []string{"sk-proj-", "sk-svcacct-", "sk-admin-", "T3BlbkFJ"}, `sk-(?:proj|svcacct|admin)-[0-9A-Za-z_\-]{20,}|sk-[0-9A-Za-z]{20}T3BlbkFJ[0-9A-Za-z]{20}`),
	tok("openrouter-key", []string{"sk-or-v1-"}, `sk-or-v1-[0-9a-f]{64}`),
	tok("slack-token", []string{"xox"}, `xox[abeoprs]-[0-9A-Za-z\-]{10,}`),
	tok("slack-app-token", []string{"xapp-"}, `xapp-[0-9]-[0-9A-Za-z]+-[0-9]+-[0-9A-Za-z]+`),
	tok("slack-webhook", []string{"hooks.slack.com"}, `hooks\.slack\.com/(?:services|workflows|triggers)/[0-9A-Za-z+/]{20,}`),
	tok("stripe-key", []string{"sk_live_", "rk_live_", "sk_test_", "rk_test_", "sk_prod_", "rk_prod_", "whsec_"}, `(?:[sr]k_(?:live|test|prod)|whsec)_[0-9A-Za-z]{16,}`),
	tok("google-api-key", []string{"AIza"}, `AIza[0-9A-Za-z_\-]{35}`),
	tok("google-oauth-secret", []string{"GOCSPX-"}, `GOCSPX-[0-9A-Za-z_\-]{28}`),
	tok("google-oauth-token", []string{"ya29."}, `ya29\.[0-9A-Za-z_\-]{30,}`),
	tok("npm-token", []string{"npm_"}, `npm_[0-9A-Za-z]{36}`),
	tok("pypi-token", []string{"pypi-AgEIcHlwaS5vcmc"}, `pypi-AgEIcHlwaS5vcmc[0-9A-Za-z_\-]{50,}`),
	tok("huggingface-token", []string{"hf_"}, `hf_[A-Za-z]{34}`),
	tok("sendgrid-key", []string{"SG."}, `SG\.[0-9A-Za-z_\-]{22}\.[0-9A-Za-z_\-]{43}`),
	tok("digitalocean-token", []string{"dop_v1_", "doo_v1_", "dor_v1_"}, `do[por]_v1_[0-9a-f]{64}`),
	tok("tailscale-key", []string{"tskey-"}, `tskey-[a-z]+-[0-9A-Za-z]+-[0-9A-Za-z]{16,}`),
	tok("databricks-token", []string{"dapi"}, `dapi[0-9a-f]{32}(?:-[0-9])?`),
	tok("linear-key", []string{"lin_api_"}, `lin_api_[0-9A-Za-z]{40}`),
	tok("age-secret-key", []string{"AGE-SECRET-KEY-1"}, `AGE-SECRET-KEY-1[QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L]{58}`),
	tok("onepassword-token", []string{"ops_eyJ"}, `ops_eyJ[0-9A-Za-z+/]{100,}={0,3}`),
	tok("flopwire-token", []string{"flopwire_"}, `flopwire_[0-9A-Za-z_\-]{43}`),
	tok("jwt", []string{"eyJ"}, `eyJ[0-9A-Za-z_\-]{10,}\.eyJ[0-9A-Za-z_\-]{10,}\.[0-9A-Za-z_\-]{16,}`),
	{
		// Azure storage connection strings.
		id: "azure-account-key", keywords: []string{"AccountKey="}, hashed: true,
		re: regexp.MustCompile(`AccountKey=([0-9A-Za-z+/]{80,90}={0,2})`), group: 1,
	},
	{
		// scheme://user:password@host: only the password is masked.
		id: "url-password", keywords: []string{"://"},
		re:       regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]{1,20}://[^\s:/@"'\\<>]{0,64}:([^\s:/@"'\\<>]{1,128})@([0-9A-Za-z\[][0-9A-Za-z.\-\[\]:]{0,253})`),
		group:    1,
		validate: urlPasswordOK,
		// A credential for a database on the developer's own machine
		// (postgres://app:devpass@localhost) guards nothing.
		skip: func(sub [][]byte) bool { return loopback(sub[2]) },
	},
	{
		// Authorization / Proxy-Authorization header values and bare
		// Bearer tokens.
		id: "auth-header", keywords: []string{"authorization", "bearer "},
		re:       regexp.MustCompile(`(?i)(?:authorization\\?["']?\s{0,3}[:=]\s{0,3}\\?["']?\s{0,3}(?:bearer|basic|token|bot|digest)|\bbearer)\s+([0-9A-Za-z._~+/=\-]{16,})`),
		group:    1,
		validate: credentialValueOK,
	},
	{
		// Cookie and Set-Cookie header values (to the end of the header).
		id: "cookie-header", keywords: []string{"cookie"},
		re:       regexp.MustCompile(`(?i)\b(?:set-)?cookie\\?["']?\s{0,3}:\s{0,3}\\?["']?([^\r\n"'\\]{8,1000})`),
		group:    1,
		validate: cookieOK,
	},
}

// windows: per rule, how far the regex reaches around its keyword.
var windows = map[string][2]int{
	"private-key":       {100, 8192},
	"openai-key":        {48, 200},
	"jwt":               {0, 4096},
	"onepassword-token": {0, 2048},
	"url-password":      {24, 256},
	"auth-header":       {16, 1200},
	"cookie-header":     {16, 1100},
	"slack-webhook":     {0, 200},
}

func init() {
	defer func() {
		for i := range rules {
			r := &rules[i]
			r.back, r.fwd = 0, 400
			if w, ok := windows[r.id]; ok {
				r.back, r.fwd = w[0], w[1]
			}
			r.ci = r.id == "auth-header" || r.id == "cookie-header"
		}
		buildAutomaton()
	}()
	// PEM private key blocks: mask the body between the BEGIN and END
	// lines (or, for a block cut off before its END line, the base64 run
	// after BEGIN). Newlines may be real or JSON-escaped.
	rules[0] = rule{
		id: "private-key", keywords: []string{"PRIVATE KEY"}, hashed: true,
		re: regexp.MustCompile(`-----BEGIN[ A-Z0-9_-]{0,64}PRIVATE KEY(?: BLOCK)?-----((?:\\+[nrt]|[\s0-9A-Za-z+/=:,\-.]){40,}?)-----END[ A-Z0-9_-]{0,64}PRIVATE KEY(?: BLOCK)?-----` +
			`|-----BEGIN[ A-Z0-9_-]{0,64}PRIVATE KEY(?: BLOCK)?-----((?:\\+[nrt]|[\s0-9A-Za-z+/=]){64,})`),
		group:    -1, // first matching group of 1 and 2
		validate: pemBodyOK,
	}
}

// pemBodyOK: at least 40 base64 characters, so a documentation example
// ("-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----") stays.
func pemBodyOK(v []byte) bool {
	n := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\\' && i+1 < len(v) {
			i++
			continue
		}
		if isAlnum(c) || c == '+' || c == '/' {
			n++
		}
	}
	return n >= 40
}

// defaultPasswords are stock development passwords (postgres://postgres:
// postgres@localhost); masking them hides nothing and hurts search.
var defaultPasswords = map[string]bool{
	"postgres": true, "password": true, "pass": true, "pw": true, "pwd": true, "test": true, "secret": true, "root": true,
	"admin": true, "user": true, "guest": true, "dev": true, "local": true, "mysql": true, "redis": true,
}

// loopback: host (with an optional :port) is this machine.
func loopback(host []byte) bool {
	h := string(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "0.0.0.0", "[::1]", "::1", "host.docker.internal":
		return true
	}
	return false
}

func urlPasswordOK(v []byte) bool {
	if bytes.ContainsFunc(v, func(r rune) bool { return r >= 0x80 }) {
		return false // "user:…@host" is a display, not a password
	}
	if isMarker(v) || isPlaceholder(v) || defaultPasswords[string(bytes.ToLower(v))] {
		return false
	}
	switch v[0] {
	case '$', '%', '{', '*':
		return false
	}
	return true
}

func credentialValueOK(v []byte) bool {
	return !isMarker(v) && !isPlaceholder(v) && hasDigitOrSymbol(v) && entropy(v) >= 3.0
}

// cookieOK: a real header value ("name=value; ..."), not code that builds
// one (template literals, calls) or prose. The first value must look
// random.
func cookieOK(v []byte) bool {
	if isMarker(v) || bytes.Contains(v, []byte("[REDACTED")) || bytes.ContainsAny(v, "$`{}()<>") {
		return false
	}
	eq := bytes.IndexByte(v, '=')
	if eq <= 0 {
		return false
	}
	for _, c := range v[:eq] {
		if !keyByte(c) {
			return false
		}
	}
	val := v[eq+1:]
	if i := bytes.IndexAny(val, "; "); i >= 0 {
		val = val[:i]
	}
	return len(val) >= 8 && entropy(val) >= 3.0
}
