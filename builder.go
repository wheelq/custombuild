package custombuild

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Builder is a type that is able of producing a certain custom build.
type Builder struct {
	// The path to the root of the original repository
	RepoPath string

	// The function that can change the code to prepare a custom build
	Generator CodeGenFunc

	// The list of packages required for this custom build
	Packages []string

	// Length of time on average to allow each package during go get -u
	timePerPackage time.Duration

	// Path to temporary folder of the copy of the repository
	repoCopy string

	// Flag to ensure setup only occurs once
	ready bool
}

// New creates a new Builder and calls Setup at the same time. This function is
// blocking. If it returns without error, it is prepared to be used to build.
func New(repo string, codegen CodeGenFunc, dependencies []string) (Builder, error) {
	builder := Builder{
		RepoPath:       repo,
		Generator:      codegen,
		Packages:       dependencies,
		timePerPackage: defaultGoGetTimeout,
	}
	return builder, builder.Setup()
}

// Setup sets up the builder. It downloads/updates the packages and copies
// the repository to a temporary directory, where code modifications occur.
// This function is blocking. When it completes, if there is no error, it
// is ready to produce builds.
func (b *Builder) Setup() error {
	if b.ready {
		return errors.New("already set up")
	}

	// Run `go get -u` on the dependencies for this build
	err := b.goGet(b.Packages)
	if err != nil {
		return err
	}

	// Make a temporary directory in which to modify the repository
	b.repoCopy, err = ioutil.TempDir("", fmt.Sprintf("custombuild_%d_", rand.Intn(9999)))
	if err != nil {
		return err
	}

	// Copy the repository to temporary directory
	err = deepCopy(b.RepoPath, b.repoCopy)
	if err != nil {
		return err
	}

	// Mutate the code
	err = b.Generator(b.repoCopy, b.Packages)
	if err != nil {
		return err
	}

	b.ready = true
	return nil
}

// goGet runs `go get -u -d -f` for all the packages in pkgs.
// This function is blocking. If an error was returned, not all
// packages were updated. The process will be killed if it
// takes too long, which will then return an error.
func (b *Builder) goGet(pkgs []string) error {
	if len(pkgs) == 0 {
		// nothing to do
		return nil
	}

	// Set timeout
	timeout := b.timePerPackage * time.Duration(len(pkgs))
	if timeout == 0 {
		timeout = defaultGoGetTimeout
	}

	// Prepare command
	args := append([]string{"get", "-u", "-d", "-f"}, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr

	// Start process
	err := cmd.Start()
	if err != nil {
		return err
	}

	// Wait for it to exit
	done := make(chan error, 1) // buffer allows goroutine to exit immediately when cmd exits
	go func() {
		done <- cmd.Wait()
	}()

	// Or kill the process if it runs too long
	select {
	case <-time.After(timeout):
		err := cmd.Process.Kill()
		<-done
		if err != nil {
			return err
		}
		return errors.New("process killed: go get took too long")
	case err := <-done:
		if err != nil {
			return err
		}
	}

	return nil
}

// Teardown cleans up the assets that were created by a call to Setup.
func (b *Builder) Teardown() error {
	if !b.ready {
		return errors.New("not set up")
	}
	return os.RemoveAll(b.repoCopy)
}

// Build does a custom build for goos and goarch. It plops the binary
// at a file path specified by output. If arch == "arm", the default
// ARM version is used.
func (b *Builder) Build(goos, goarch, output string) error {
	if !b.ready {
		return errors.New("not set up")
	}
	destination, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", destination)
	cmd.Dir = b.repoCopy
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
	return cmd.Run()
}

// BuildARM does a custom ARM build for goos using the specified ARM version.
// It plops the binary at a file path specified by output.
func (b *Builder) BuildARM(goos string, arm int, output string) error {
	if !b.ready {
		return errors.New("not set up")
	}
	destination, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", destination)
	cmd.Dir = b.repoCopy
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm", "GOARM="+strconv.Itoa(arm))
	return cmd.Run()
}

// deepCopy makes a deep file copy of src into dest, overwriting any existing files.
// If an error occurs, not all files were copied successfully. This function blocks.
func deepCopy(src string, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		// error accessing current file
		if err != nil {
			return err
		}

		// don't copy hidden/system files or files without a name.
		if info.Name() == "" || info.Name()[0] == '.'{
			return nil
		}

		// if directory, create destination directory.
		if info.IsDir() {
			subdir := strings.TrimPrefix(path, src)
			destdir := filepath.Join(dest, subdir)
			return os.MkdirAll(destdir, info.Mode()&os.ModePerm)
		}

		// open source file
		fsrc, err := os.Open(path)
		if err != nil {
			return err
		}

		// open destination file
		destpath := strings.TrimPrefix(path, src)
		fdest, err := os.OpenFile(destpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode()&os.ModePerm)
		if err != nil {
			return err
		}

		// Copy the file and flush it to disk
		if _, err = io.Copy(fdest, fsrc); err != nil {
			fsrc.Close()
			fdest.Close()
			return err
		}
		if err = fdest.Sync(); err != nil {
			fsrc.Close()
			fdest.Close()
			return err
		}

		// Close cleanly
		if err = fsrc.Close(); err != nil {
			fdest.Close()
			return err
		}
		if err = fdest.Close(); err != nil {
			return err
		}
		return nil
	})
}

// CodeGenFunc is a function that generates/mutates Go code to
// customize a build. It receives the path to a repository and
// packages that are needed as dependencies.
type CodeGenFunc func(repo string, packages []string) error

// defaultGoGetTimeout is the duration that `go get -u` is allowed
// to run, on average, per package.
const defaultGoGetTimeout = 5 * time.Second
