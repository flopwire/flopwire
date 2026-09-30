package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/domain"
)

// adminCmd is `flopwire admin devices|revoke-device|revoke-user|token-ttl`
// (credentials) and `flopwire admin policy preview|purge`: the stored
// sessions a path-rule change hid (D18), and their confirmed purge.
func adminCmd(ctx context.Context, args []string) error {
	const use = "usage: flopwire admin devices [--json] | admin revoke-device ID | admin revoke-user ID | admin token-ttl [DURATION] | admin policy preview [--json] | admin policy purge [--rule RULE] --yes"
	if len(args) > 0 {
		switch args[0] {
		case "devices":
			return adminDevices(ctx, args[1:], os.Stdout)
		case "revoke-device":
			return revokeDevice(ctx, args[1:])
		case "revoke-user", "revoke-principal":
			return adminRevokeUser(ctx, args[1:])
		case "token-ttl":
			return adminTokenTTL(ctx, args[1:])
		}
	}
	if len(args) < 2 || args[0] != "policy" {
		return errors.New(use)
	}
	switch args[1] {
	case "preview", "status":
		fs := flag.NewFlagSet("admin policy preview", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		var h domain.HiddenSummary
		if err := c.JSON(ctx, "GET", "/v1/admin/policy/hidden", nil, &h); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(h)
		}
		writeHidden(os.Stdout, h)
		return nil
	case "purge":
		fs := flag.NewFlagSet("admin policy purge", flag.ContinueOnError)
		rule := fs.String("rule", "", "purge only the sessions this rule hid (as preview prints it)")
		yes := fs.Bool("yes", false, "confirm: the hidden sessions are deleted for good")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if !*yes {
			return errors.New("purging hidden sessions deletes them for good; run flopwire admin policy preview, then pass --yes")
		}
		c, err := adminClient()
		if err != nil {
			return err
		}
		var out map[string]int
		if err := c.JSON(ctx, "POST", "/v1/admin/policy/hidden/purge", map[string]any{"rule": *rule, "confirm": true}, &out); err != nil {
			return err
		}
		fmt.Printf("purged %d hidden sessions", out["purged"])
		if out["restored"] > 0 {
			fmt.Printf("; restored %d the rules no longer cover", out["restored"])
		}
		fmt.Println()
		return nil
	}
	return errors.New(use)
}

func adminClient() (client.HTTP, error) {
	cfg, err := client.Load()
	if err != nil {
		return client.HTTP{}, err
	}
	token, err := cfg.SessionCredential()
	if err != nil {
		return client.HTTP{}, err
	}
	return cfg.API(token), nil
}

// writeHidden prints a hidden-session preview.
func writeHidden(w io.Writer, h domain.HiddenSummary) {
	if h.Total == 0 {
		fmt.Fprintln(w, "no sessions are hidden by path rules")
		return
	}
	fmt.Fprintf(w, "%d sessions hidden by path rules (purged %s after hiding unless a rule change restores them", h.Total, h.PurgeAfter)
	if h.OldestHiddenAt != nil {
		fmt.Fprintf(w, "; oldest hidden %s", h.OldestHiddenAt.Local().Format(time.RFC3339))
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w, "by rule:")
	for _, r := range h.ByRule {
		fmt.Fprintf(w, "  %6d  %s\n", r.Sessions, r.Rule)
	}
	fmt.Fprintln(w, "by user:")
	for _, u := range h.ByUser {
		fmt.Fprintf(w, "  %6d  %s\n", u.Sessions, u.Email)
	}
}

// adminDevices lists devices: owner, label, kind, created, last seen, last
// IP, expiry and scopes.
func adminDevices(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("admin devices", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := adminClient()
	if err != nil {
		return err
	}
	var out struct {
		Devices []domain.Device `json:"devices"`
	}
	if err := c.JSON(ctx, "GET", "/v1/admin/devices", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	writeDevices(w, out.Devices, time.Now())
	return nil
}

func writeDevices(w io.Writer, devices []domain.Device, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSER\tLABEL\tKIND\tCREATED\tLAST SEEN\tLAST IP\tEXPIRES\tSCOPES")
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	for _, d := range devices {
		label := d.Label
		if label == "" {
			label = d.Name
		}
		expires := stamp(d.ExpiresAt)
		switch {
		case !d.RevokedAt.IsZero():
			expires = "revoked " + stamp(d.RevokedAt)
		case !d.ExpiresAt.IsZero() && !now.Before(d.ExpiresAt):
			expires = "expired " + expires
		}
		scopes := strings.Join(d.Scopes, ",")
		if scopes == "" {
			scopes = "-"
		}
		ip := d.LastIP
		if ip == "" {
			ip = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.UserEmail, label, d.Kind, stamp(d.CreatedAt), stamp(d.LastSeen), ip, expires, scopes)
	}
	tw.Flush()
}

// adminRevokeUser revokes every credential of a user or service identity:
// its sessions, device credentials and the tokens it minted.
func adminRevokeUser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("admin revoke-user", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: flopwire admin revoke-user USER_ID")
	}
	c, err := adminClient()
	if err != nil {
		return err
	}
	var out map[string]any
	if err := c.JSON(ctx, "POST", "/v1/admin/users/"+url.PathEscape(fs.Arg(0))+"/revoke", nil, &out); err != nil {
		return err
	}
	return printJSON(out)
}

// adminTokenTTL shows or sets the cap on minted tokens' TTL (0: the
// default, 24h).
func adminTokenTTL(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: flopwire admin token-ttl [DURATION]")
	}
	c, err := adminClient()
	if err != nil {
		return err
	}
	var policy domain.Policy
	if err := c.JSON(ctx, "GET", "/v1/policy", nil, &policy); err != nil {
		return err
	}
	if len(args) == 1 {
		d, err := time.ParseDuration(args[0])
		if err != nil || d < 0 {
			return errors.New("token-ttl takes a duration such as 24h (0 restores the default)")
		}
		secs := int64(d / time.Second)
		body := map[string]any{"max_storage_bytes": policy.MaxStorageBytes, "max_user_bytes": policy.MaxUserBytes, "path_rules": policy.PathRules,
			"unplaceable": policy.Unplaceable, "max_token_ttl_seconds": secs}
		if err := c.JSON(ctx, "PUT", "/v1/admin/policy", body, &policy); err != nil {
			return err
		}
	}
	fmt.Printf("minted tokens live at most %s\n", policy.MaxTokenTTL())
	return nil
}
