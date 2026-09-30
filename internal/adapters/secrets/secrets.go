// Package secrets is a driven adapter implementing ports.SecretResolver.
//
// BYOK means the operator's credentials stay the operator's. Agentd reads them
// from wherever they already keep them, holds them for the length of one fetch,
// and never writes them anywhere: not to the database, not into a capture, not
// into an audit event, not into a log line. The core only ever handles
// domain.SecretRef, which is a name, and domain.Secret, whose String method is
// redacted so that a stray %v cannot leak one.
package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Env resolves secrets from environment variables.
//
// A reference "api-token" is looked up as AGENTD_SECRET_API_TOKEN by default.
// The prefix exists so that Agentd cannot be talked into reading an arbitrary
// environment variable by a check definition naming, say, "PATH".
type Env struct {
	// Prefix is prepended to the upper-cased reference. Empty means
	// DefaultEnvPrefix.
	Prefix string
}

// DefaultEnvPrefix scopes which environment variables Agentd will read.
const DefaultEnvPrefix = "AGENTD_SECRET_"

var _ ports.SecretResolver = Env{}

// Resolve looks each reference up. A missing one is an auth-class failure:
// retrying will not conjure a credential, so saying "transient" would make
// Agentd hammer a source it can never authenticate to.
func (e Env) Resolve(_ context.Context, refs []domain.SecretRef) (domain.SecretBundle, error) {
	prefix := e.Prefix
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}

	bundle := make(domain.SecretBundle, len(refs))
	var missing []string
	for _, ref := range refs {
		name := prefix + envName(string(ref))
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			missing = append(missing, string(ref))
			continue
		}
		bundle[ref] = domain.NewSecret(value)
	}

	if len(missing) > 0 {
		// The reference names are safe to report; the values are what must
		// never appear.
		return nil, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "secret_missing",
			Summary: "this check needs credentials that are not configured: " + strings.Join(missing, ", "),
		}
	}
	return bundle, nil
}

// envName turns "api-token" into "API_TOKEN".
func envName(ref string) string {
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Dir resolves secrets from a directory of files, one per reference.
//
// This is the shape Docker secrets, Kubernetes projected volumes and systemd
// credentials all use, and it is better than the environment for anything
// long-lived: a file has permissions, and it does not get inherited by every
// child process Agentd starts -- which matters because Agentd starts MCP
// plugins as child processes.
type Dir struct {
	// Path is the directory holding the secrets.
	Path string

	mu     sync.Mutex
	cached map[domain.SecretRef]domain.Secret
}

var _ ports.SecretResolver = (*Dir)(nil)

// Resolve reads each reference from a file named after it.
func (d *Dir) Resolve(_ context.Context, refs []domain.SecretRef) (domain.SecretBundle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached == nil {
		d.cached = map[domain.SecretRef]domain.Secret{}
	}

	bundle := make(domain.SecretBundle, len(refs))
	var missing []string

	for _, ref := range refs {
		if s, ok := d.cached[ref]; ok {
			bundle[ref] = s
			continue
		}

		// Reject anything that could escape the directory. A check definition
		// is operator-authored, but it is still configuration, and
		// configuration should not be able to name ../../etc/shadow.
		name := string(ref)
		if name == "" || strings.ContainsAny(name, `/\`) || name == ".." {
			missing = append(missing, name)
			continue
		}

		filePath := filepath.Join(d.Path, name)
		if fi, err := os.Stat(filePath); err == nil {
			if runtime.GOOS != "windows" && (fi.Mode().Perm()&0004 != 0) {
				return nil, domain.Failure{
					Class:   domain.ClassAuth,
					Code:    "insecure_secret_permissions",
					Summary: fmt.Sprintf("refusing to read secret %q from file %q with world-readable permissions (%04o); mode must be 0600 or 0400", name, filePath, fi.Mode().Perm()),
				}
			}
		}

		raw, err := os.ReadFile(filePath)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		s := domain.NewSecret(strings.TrimRight(string(raw), "\r\n"))
		d.cached[ref] = s
		bundle[ref] = s
	}

	if len(missing) > 0 {
		return nil, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "secret_missing",
			Summary: "this check needs credentials that are not configured: " + strings.Join(missing, ", "),
		}
	}
	return bundle, nil
}

// Chain tries several resolvers in order and returns the first value found for
// each reference, so that an operator can keep most secrets in files and
// override one from the environment without reconfiguring anything.
type Chain []ports.SecretResolver

var _ ports.SecretResolver = Chain{}

// Resolve asks each resolver for each reference in turn.
//
// One reference at a time, deliberately. A resolver that cannot supply
// everything asked of it returns an error and no bundle at all, so asking for
// the whole set would throw away the references it did have. Asking singly
// costs a few more calls and makes a partial answer usable, which is the whole
// point of a chain.
func (c Chain) Resolve(ctx context.Context, refs []domain.SecretRef) (domain.SecretBundle, error) {
	bundle := make(domain.SecretBundle, len(refs))
	var missing []domain.SecretRef

	for _, ref := range refs {
		found := false
		for _, r := range c {
			got, err := r.Resolve(ctx, []domain.SecretRef{ref})
			if err != nil {
				continue
			}
			if secret, ok := got.Get(ref); ok {
				bundle[ref] = secret
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, ref)
		}
	}

	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, ref := range missing {
			names[i] = string(ref)
		}
		return nil, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "secret_missing",
			Summary: "this check needs credentials that are not configured: " + strings.Join(names, ", "),
		}
	}
	return bundle, nil
}

// Static is an in-memory resolver, for tests and for a single-check invocation
// where the operator passes a value on the command line.
type Static map[domain.SecretRef]string

var _ ports.SecretResolver = Static{}

// Resolve returns the configured values.
func (s Static) Resolve(_ context.Context, refs []domain.SecretRef) (domain.SecretBundle, error) {
	bundle := make(domain.SecretBundle, len(refs))
	for _, ref := range refs {
		v, ok := s[ref]
		if !ok {
			return nil, domain.Failure{
				Class:   domain.ClassAuth,
				Code:    "secret_missing",
				Summary: fmt.Sprintf("this check needs a credential named %q that is not configured", ref),
			}
		}
		bundle[ref] = domain.NewSecret(v)
	}
	return bundle, nil
}
