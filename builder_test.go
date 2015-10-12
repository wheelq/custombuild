package custombuild

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestErrorFmt(t *testing.T) {
	cmd := exec.Command("go", "get", "me", "a", "sandwich")
	err := errors.New("exit status 1")
	buf := strings.NewReader("something\nfoo bar\nsomething\nyada yada")

	expectedErrMsg := `exit status 1
---- COMMAND: go get me a sandwich
---- 
---- something
---- foo bar
---- something
---- yada yada`

	actualErr := errorFmt(cmd, err, buf)

	if actualErr.Error() != expectedErrMsg {
		t.Errorf("\nExpected: '%s'\nActual: '%s'", expectedErrMsg, actualErr.Error())
	}
}
