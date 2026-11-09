package crew

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// The vault is data/crew/<id>/vault.json: name to value, each value
// encrypted with the house's key (internal/secrets), so the file can be
// backed up and read with cat without handing out the agent's accounts.
// A turn gets the values in its environment, one variable per name
// (Env): the agent uses $SMTP_PASSWORD in a command and never needs it
// written in a file it could leave lying around.

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// reservedKeys are variables a turn's environment is built with, or that
// change how every program in it behaves: a vault entry may not be one.
var reservedKeys = map[string]bool{"PATH": true, "HOME": true, "SHELL": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TERM": true, "PWD": true, "TMPDIR": true, "IFS": true, "ENV": true, "BASH_ENV": true, "NODE_OPTIONS": true, "PYTHONPATH": true, "PYTHONSTARTUP": true, "PORT": true}

func validKey(k string) error {
	if !keyRe.MatchString(k) {
		return fmt.Errorf("%q is not a vault name: capitals, digits and _, starting with a letter", k)
	}
	if reservedKeys[k] || strings.HasPrefix(k, "MIDDLEWARE_") || strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "OPENAI_") || strings.HasPrefix(k, "GIT_") {
		return fmt.Errorf("%s is a name the house or the agent's own tools use; pick another", k)
	}
	return nil
}

func (c *Crew) vaultPath(id string) string { return filepath.Join(c.dir(id), "vault.json") }

func (c *Crew) readVault(id string) map[string]string {
	v := map[string]string{}
	_ = fsx.ReadJSON(c.vaultPath(id), &v)
	return v
}

func (c *Crew) vaultKeys(id string) []string {
	v := c.readVault(id)
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setVaultLocked writes values into the vault, encrypted.
func (c *Crew) setVaultLocked(id string, vals map[string]string) error {
	if c.opts.Box == nil {
		return errors.New("the house has no key to encrypt the vault with")
	}
	v := c.readVault(id)
	for k, val := range vals {
		if err := validKey(k); err != nil {
			return err
		}
		enc, err := c.opts.Box.Encrypt(val)
		if err != nil {
			return err
		}
		v[k] = enc
	}
	if len(v) > 200 {
		return errors.New("a vault holds 200 values at most")
	}
	return fsx.WriteJSON(c.vaultPath(id), v, 0o600)
}

// SetSecret writes one value; the operator's, from the panel.
func (c *Crew) SetSecret(id, key, value string) error {
	key = strings.ToUpper(strings.TrimSpace(key))
	if len(value) > maxFieldVal {
		return fmt.Errorf("the value is longer than %d bytes", maxFieldVal)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return ErrNoAgent
	}
	if err := c.setVaultLocked(id, map[string]string{key: value}); err != nil {
		return err
	}
	c.note(mb, Entry{Kind: KindVault, Message: "the operator set " + key})
	return nil
}

// DeleteSecret removes one value.
func (c *Crew) DeleteSecret(id, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return ErrNoAgent
	}
	v := c.readVault(id)
	if _, ok := v[key]; !ok {
		return fmt.Errorf("%s is not in the vault", key)
	}
	delete(v, key)
	if err := fsx.WriteJSON(c.vaultPath(id), v, 0o600); err != nil {
		return err
	}
	c.note(mb, Entry{Kind: KindVault, Message: "the operator removed " + key})
	return nil
}

// Env is the vault in clear, for a turn's environment and the house's own
// calls on the agent's behalf (its mail). A value that does not decrypt
// is left out rather than handed over as ciphertext.
func (c *Crew) Env(id string) map[string]string {
	out := map[string]string{}
	if c.opts.Box == nil {
		return out
	}
	for k, enc := range c.readVault(id) {
		if validKey(k) != nil {
			continue
		}
		if v, err := c.opts.Box.Decrypt(enc); err == nil {
			out[k] = v
		}
	}
	return out
}
