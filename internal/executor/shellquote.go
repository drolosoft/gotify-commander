package executor

import (
	"regexp"
	"strings"
)

// plainShellWord matches words that no POSIX shell treats specially, so they
// can be sent as they are and stay readable in logs.
var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// shellQuote returns word as exactly one POSIX shell word. Anything that is not
// a plain word is wrapped in single quotes, inside which the shell expands
// nothing; a single quote in the word closes the quoting, adds an escaped
// quote and opens it again.
func shellQuote(word string) string {
	if word == "" {
		return "''"
	}

	if plainShellWord.MatchString(word) {
		return word
	}

	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// remoteCommand joins name and args into the command line an SSH server hands
// to the user's shell, quoting each one so that no argument can add a second
// command or expand anything.
func remoteCommand(name string, args ...string) string {
	words := make([]string, 0, 1+len(args))
	words = append(words, shellQuote(name))

	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}

	return strings.Join(words, " ")
}
