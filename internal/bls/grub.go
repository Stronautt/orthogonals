package bls

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// GrubDefaultsPath is what grub2-mkconfig sources. Fedora regenerates
// /etc/kernel/cmdline — and, under GRUB_ENABLE_BLSCFG, the entries' options
// line — from GRUB_CMDLINE_LINUX here, so an arg written only to the two
// derived targets is dropped by the next package update that regenerates them.
const GrubDefaultsPath = "/etc/default/grub"

// GRUB_CMDLINE_LINUX_DEFAULT is not a target: Fedora does not ship it, and
// where it exists it is additive to GRUB_CMDLINE_LINUX rather than a
// replacement, so args in GRUB_CMDLINE_LINUX already reach the boot path.
const grubCmdlineKey = "GRUB_CMDLINE_LINUX"

// grubExpandChars are what the shell would re-read on the way out of this file:
// splitting on whitespace and rejoining changes what $(…), a backtick or an
// escape produces. Quotes are not here — unquoted decides those, and it has to
// tell a wrapped value from a stray quote to do it.
const grubExpandChars = "$`\\"

// The reasons a line is refused. Each names what the parser could not model, so
// the remedy is about that line rather than about the boot config in general.
const (
	whyForm      = "an assignment form orthogonals does not manage"
	whyQuoting   = "a quote orthogonals cannot split the value on"
	whyExpansion = "a shell expansion"
)

// GrubError is a line this package will not edit. Refusing is the whole
// contract: a value spliced into a line the parser misread changes what
// grub2-mkconfig puts on the kernel command line, and the host boots on it.
type GrubError struct {
	Line int    // 1-based
	Text string // the offending line, trimmed
	Why  string
}

func (e *GrubError) Error() string {
	return fmt.Sprintf("%s line %d carries %s — orthogonals cannot edit it without changing what the shell reads: %s",
		GrubDefaultsPath, e.Line, e.Why, e.Text)
}

// GrubRemedy is the fix for every GrubError, so the CLI and preflight cannot
// drift apart on it.
const GrubRemedy = "rewrite that line as a plain " + grubCmdlineKey +
	`="…" assignment with no $, backtick, backslash or embedded quote, then re-run`

// grubTarget is /etc/default/grub. A host without one contributes no target:
// grub2-mkconfig is not what regenerates its boot config, and a file this
// package invented would carry nothing but our own args the first time someone
// did run grub2-mkconfig.
//
// A file that exists without the key is a target — there the regenerator is
// real and currently supplies nothing.
func grubTarget(root string) (*target, error) {
	path := filepath.Join(root, GrubDefaultsPath)
	lines, mode, err := fileLines(path)
	if err != nil || lines == nil {
		return nil, err
	}
	idx, value, quote, err := grubAssign(lines)
	if err != nil {
		return nil, err
	}
	return &target{
		rel: GrubDefaultsPath, path: path, mode: mode,
		was:    []byte(strings.Join(lines, "\n")),
		tokens: strings.Fields(value),
		render: func(toks []string) ([]byte, error) { return renderGrub(lines, idx, quote, toks) },
	}, nil
}

// grubAssign locates the effective GRUB_CMDLINE_LINUX assignment: the last
// uncommented one, since a later assignment wins under sh.
//
// idx is -1 only when no uncommented line names the variable at all, which is
// the one state where renderGrub may append its own. Any other mention is
// refused rather than skipped: appending past an `export GRUB_CMDLINE_LINUX=`
// or a lone `+=` emits an assignment that wins under sh and discards whatever
// the host booted with, down to its rd.luks.uuid=.
func grubAssign(lines []string) (idx int, value string, quote byte, err error) {
	idx = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || !namesCmdlineKey(t) {
			continue
		}
		rest, ok := strings.CutPrefix(t, grubCmdlineKey+"=")
		if !ok {
			return -1, "", 0, &GrubError{i + 1, t, whyForm}
		}
		v, q, ok := unquoted(rest)
		if !ok {
			return -1, "", 0, &GrubError{i + 1, t, whyQuoting}
		}
		if strings.ContainsAny(v, grubExpandChars) {
			return -1, "", 0, &GrubError{i + 1, t, whyExpansion}
		}
		idx, value, quote = i, v, q
	}
	return idx, value, quote, nil
}

// namesCmdlineKey reports whether the line names GRUB_CMDLINE_LINUX itself. The
// boundary check is load-bearing: without it every host carrying
// GRUB_CMDLINE_LINUX_DEFAULT — additive, deliberately not a target — is refused.
func namesCmdlineKey(line string) bool {
	for i := 0; i < len(line); {
		j := strings.Index(line[i:], grubCmdlineKey)
		if j < 0 {
			return false
		}
		end := i + j + len(grubCmdlineKey)
		if end == len(line) || !identByte(line[end]) {
			return true
		}
		i = end
	}
	return false
}

func identByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// renderGrub rewrites the assignment's value, splicing between the quotes rather
// than re-rendering the line, and appends the assignment when the file carries
// none.
func renderGrub(lines []string, idx int, quote byte, toks []string) ([]byte, error) {
	value := strings.Join(toks, " ")
	// The tokens reaching here come from the entries as well as from a caller,
	// so this is not a restatement of the read guard: it stops a token the
	// shell would re-read from being written into a file the shell sources.
	if strings.ContainsAny(value, grubExpandChars+`"'`) {
		return nil, fmt.Errorf("refusing to write %s=%q to %s: the shell would not read it back unchanged",
			grubCmdlineKey, value, GrubDefaultsPath)
	}
	// An existing assignment keeps the quoting it was written with, except that
	// a value grown past one token needs quotes to stay one word. A line this
	// package authors takes the distro's convention instead.
	if idx < 0 || (quote == 0 && strings.ContainsAny(value, " \t")) {
		quote = '"'
	}
	out := slices.Clone(lines)
	if idx < 0 {
		// Nothing to say and no line to say it on. Undoing an add that authored
		// the assignment still leaves it behind, emptied: an empty assignment
		// and an absent one mean the same thing to grub2-mkconfig, and deleting
		// it would delete the line on a host that shipped an empty one.
		if value == "" {
			return []byte(strings.Join(out, "\n")), nil
		}
		return []byte(strings.Join(appendLine(out, grubCmdlineKey+"="+quoted(value, quote)), "\n")), nil
	}
	out[idx] = grubCmdlineKey + "=" + quoted(value, quote)
	return []byte(strings.Join(out, "\n")), nil
}

// unquoted strips a matched pair of surrounding quotes. ok is false when the
// value is not one shell word: a trailing comment, an unbalanced quote and an
// escaped quote inside the value all land here, and splicing a token into any
// of them would move where the assignment ends.
func unquoted(s string) (string, byte, bool) {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		inner := s[1 : len(s)-1]
		if strings.ContainsRune(inner, rune(s[0])) {
			return "", 0, false
		}
		return inner, s[0], true
	}
	if strings.ContainsAny(s, `"'`) {
		return "", 0, false
	}
	return s, 0, true
}

func quoted(s string, quote byte) string {
	if quote == 0 {
		return s
	}
	return string(quote) + s + string(quote)
}
