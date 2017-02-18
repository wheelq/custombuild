package custombuild

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/go/ast/astutil"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Builder is a type that is able of producing a certain custom build.
type Builder struct {
	// The path to the root of the original source
	RepoPath string

	// The function that can change the code to prepare a custom build
	Generator CodeGenFunc

	// The list of packages required for this custom build
	Packages []string

	// CommandName is the command to execute to perform the builds.
	// If not set, will default to "go" (go build). Useful if
	// using own build script.
	CommandName string

	// CommandArgs is the list of args to pass to CommandName. Will
	// only be used if CommandName is set. NOTE: The destination
	// (output) of the built file will always be appended as the last
	// argument, and then any custom arguments for that build will
	// be appended (as passed into the actual Build* method).
	CommandArgs []string

	// Subpackage to build.
	// If empty, topmost package is built.
	SubPackage string

	// Length of time on average to allow each package during go get -u
	timePerPackage time.Duration

	// Path to temporary folder of the copy of the repository
	repoCopy string

	// GOPATH to use for Generator
	goPath string

	// Flag to check if -u should be used with go get
	useNetworkForAll bool

	// Flag to ensure setup only occurs once
	ready bool

	// Environment variables
	env Env
}

// New creates a new Builder and calls Setup at the same time. This function is
// blocking. If it returns without error, it is prepared to be used to build.
// src can be path to source folder or path relative to GOPATH.
func New(src string, codegen CodeGenFunc, dependencies []string) (Builder, error) {
	builder, err := NewUnready(src, codegen, dependencies)
	if err != nil {
		return builder, err
	}
	return builder, builder.Setup()
}

// NewUnready does same thing as New but unlike New, does not call Setup.
// This is useful to modify some configurations before Setup. Setup must
// still be called before building.
func NewUnready(src string, codegen CodeGenFunc, dependencies []string) (Builder, error) {
	env := Env(os.Environ())
	repo, err := validateSrc(env, src)
	if err != nil {
		return Builder{}, err
	}
	return Builder{
		RepoPath:         repo,
		Generator:        codegen,
		Packages:         dependencies,
		timePerPackage:   defaultGoGetTimeout,
		useNetworkForAll: true,
		env:              env,
	}, nil
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

	randInt := rand.Intn(9999)
	// Make a temporary GOPATH
	b.goPath, err = ioutil.TempDir("", fmt.Sprintf("custombuild_%d_", randInt))
	if err != nil {
		return err
	}

	// prepend GOPATH with src directory to prevent import path issues
	b.env.Set("GOPATH", b.goPath+string(filepath.ListSeparator)+b.env.Get("GOPATH"))

	b.repoCopy = filepath.Join(b.goPath, "src", fmt.Sprintf("%s_%d_", filepath.Base(b.RepoPath), randInt))
	// Create src directory
	err = os.MkdirAll(b.repoCopy, os.FileMode(0700))
	if err != nil {
		return err
	}

	// Copy the repository to temporary directory
	err = DeepCopy(b.RepoPath, b.repoCopy)
	if err != nil {
		return err
	}

	// Mutate the code
	if b.Generator != nil {
		err = b.Generator(b.repoCopy, b.Packages)
		if err != nil {
			return err
		}
	}

	b.ready = true
	return nil
}

// UseNetworkForAll sets if network should be used to fetch all package dependencies
// including previously fetched ones which basically uses -u flag for go get during Setup.
// This defaults to true. To set to false, create builder with NewUnready and set this
// to false before Setup.
func (b *Builder) UseNetworkForAll(useNetwork bool) {
	b.useNetworkForAll = useNetwork
}

// goGet runs `go get` for all the packages in pkgs.
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
	args := []string{"get", "-d"}
	if b.useNetworkForAll {
		args = append(args, "-u", "-f")
	}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	errBuf := new(bytes.Buffer)
	cmd.Stderr = errBuf
	cmd.Env = b.env

	// Start process
	err := cmd.Start()
	if err != nil {
		return errorFmt(cmd, err, errBuf)
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
			return errorFmt(cmd, err, errBuf)
		}
		return errorFmt(cmd, errors.New("process killed: go get took too long"), errBuf)
	case err := <-done:
		if err != nil {
			return errorFmt(cmd, err, errBuf)
		}
	}

	return nil
}

// errorFmt produces an error that prints nicely to a log file. It is
// for use when an external command ends unsucessfully. Pass in the cmd
// that was executed, the err it returned, and the buf containing its
// stderr (and maybe stdout if you want the full story). The log entry
// will be multiple lines but subsequent lines will be indented.
func errorFmt(cmd *exec.Cmd, err error, buf io.Reader) error {
	const prefix = "\n---- "
	result := fmt.Sprintf("%s%sCOMMAND: %s%s", err.Error(), prefix, strings.Join(cmd.Args, " "), prefix)
	scanner := bufio.NewScanner(buf)
	for scanner.Scan() {
		result += prefix + scanner.Text()
	}
	return errors.New(result)
}

// Teardown cleans up the assets that were created by a call to Setup.
func (b *Builder) Teardown() error {
	if !b.ready {
		return errors.New("not set up")
	}
	return os.RemoveAll(b.goPath)
}

// Build does a custom build for goos and goarch. It plops the binary
// at a file path specified by output. If goarch == "arm", the default
// GOARM version is used.
func (b *Builder) Build(goos, goarch, output string, args ...string) error {
	return b.build(goos, goarch, "", output, false, args...)
}

// BuildARM does a custom ARM build for goos using the specified ARM version
// in goarm. It plops the binary at a file path specified by output.
func (b *Builder) BuildARM(goos string, goarm int, output string, args ...string) error {
	return b.build(goos, "arm", strconv.Itoa(goarm), output, false, args...)
}

// BuildStatic does the same thing Build but the output is a static executable. Assumes
// the Go standard library was built with CGO_ENABLED=0.
func (b *Builder) BuildStatic(goos, goarch, output string, args ...string) error {
	return b.build(goos, goarch, "", output, true, args...)
}

// BuildStaticARM does the same thing BuildARM but the output is a static executable, assuming
// the Go standard library was built with CGO_ENABLED=0.
func (b *Builder) BuildStaticARM(goos string, goarm int, output string, args ...string) error {
	return b.build(goos, "arm", strconv.Itoa(goarm), output, true, args...)
}

func (b *Builder) build(goos, goarch, goarm, output string, static bool, args ...string) error {
	if !b.ready {
		return errors.New("not set up")
	}
	destination, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	cmdName := "go"
	cmdArgs := append([]string{"build", "-o", destination}, args...)
	if b.CommandName != "" {
		cmdName = b.CommandName
		cmdArgs = append(append(b.CommandArgs, destination), args...)
	}
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Dir = path.Join(b.repoCopy, b.SubPackage)
	errBuf := new(bytes.Buffer)
	cmd.Stderr = errBuf
	cmd.Env = append(b.env, "GOOS="+goos, "GOARCH="+goarch, "GOARM="+goarm)
	if static {
		cmd.Env = append(cmd.Env, "CGO_ENABLED=0")
	}
	if err := cmd.Run(); err != nil {
		return errorFmt(cmd, err, errBuf)
	}
	return nil
}

// SetImportPath moves the source directory to a path corresponding to
// importPath in GOPATH at runtime.
// Should be set if source directory contains subpackages.
func (b *Builder) SetImportPath(importPath string) error {
	newDirectory := filepath.Join(b.goPath, "src", importPath)

	if err := os.MkdirAll(b.baseImportPath(newDirectory), os.FileMode(0700)); err != nil {
		return err
	}

	// destination directory must not exist on Windows before renaming.
	if runtime.GOOS == "windows" {
		os.RemoveAll(newDirectory)
	}
	err := os.Rename(b.repoCopy, newDirectory)
	b.repoCopy = newDirectory
	return err
}

func (b *Builder) baseImportPath(importPath string) string {
	return strings.TrimSuffix(importPath, filepath.Base(importPath))
}

// RewriteImportsFrom rewrites import path from importPath to a path relative to
// the source directory at runtime.
func (b *Builder) RewriteImportsFrom(importPath string) error {
	newPath := filepath.Base(b.repoCopy)
	return b.RewriteImports(importPath, newPath)
}

// RewriteImports rewrites import paths equal to or prefixed with oldPath
// for source directory and subpackages from oldPath to newPath
func (b *Builder) RewriteImports(oldPath, newPath string) error {
	return filepath.Walk(b.repoCopy, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") {
			return filepath.SkipDir
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		return rewritePath(path, oldPath, newPath)
	})
}

// rewritePath rewrites import paths in file from oldPath to newPath
func rewritePath(file, oldPath, newPath string) error {
	stat, err := os.Stat(file)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return err
	}
	for _, imp := range f.Imports {
		impPath, _ := strconv.Unquote(imp.Path.Value)
		if strings.HasPrefix(impPath, oldPath) {
			subpackage := strings.TrimPrefix(impPath, oldPath)
			astutil.RewriteImport(fset, f, impPath, newPath+subpackage)
		}
	}
	ofile, err := os.OpenFile(file, os.O_RDWR|os.O_TRUNC, stat.Mode())
	if err != nil {
		return err
	}
	defer ofile.Close()
	return printer.Fprint(ofile, fset, f)
}

// DeepCopy makes a deep file copy of src into dest, overwriting any existing files.
// If an error occurs, not all files were copied successfully. This function blocks.
// Hidden/system/nameless files are skipped.
func DeepCopy(src string, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		// error accessing current file
		if err != nil {
			return err
		}

		// don't copy hidden/system files or files without a name.
		if info.Name() == "" || info.Name()[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}
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
		destpath := filepath.Join(dest, strings.TrimPrefix(path, src))
		fdest, err := os.OpenFile(destpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode()&os.ModePerm)
		if err != nil {
			fsrc.Close()
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

// validateSrc validates if src is a valid source directory. If the directory
// is not present, it checks GOPATH for the package.
// It returns the absolute path to the src directory if found.
func validateSrc(env Env, src string) (string, error) {
	// check if file exists
	if _, err := os.Stat(src); err == nil {
		return filepath.Abs(src)
	}
	// check if present in GOPATH
	if r := absFromGoPath(env.Get("GOPATH"), src); r != "" {
		return r, nil
	}
	// not valid
	return "", fmt.Errorf("Invalid source directory")
}

// absFromGoPath fetches the absolute path to repo in GOPATH.
// It returns the path if found and an empty string otherwise.
func absFromGoPath(gopath string, repo string) string {
	gopaths := strings.Split(gopath, string(filepath.ListSeparator))
	for _, gp := range gopaths {
		absPath := filepath.Join(gp, "src", repo)
		if _, err := os.Stat(absPath); err == nil {
			return absPath
		}
	}
	return ""
}

// Env represents environment variables
type Env []string

// Set sets the environment key to value.
func (e *Env) Set(key, value string) {
	keyVal := key + "=" + value
	for i, v := range *e {
		env := strings.SplitN(v, "=", 2)
		if len(env) < 2 {
			continue
		}
		if env[0] == key {
			(*e)[i] = keyVal
			break
		}
	}
	*e = append(*e, keyVal)
}

// Get retrieves the environment variable key
func (e Env) Get(key string) string {
	for _, v := range e {
		env := strings.SplitN(v, "=", 2)
		if len(env) < 2 {
			continue
		}
		if env[0] == key {
			return env[1]
		}
	}
	return ""
}

// CodeGenFunc is a function that generates/mutates Go code to
// customize a build. It receives the path to the source root and
// packages that are needed as dependencies.
type CodeGenFunc func(sourceDir string, packages []string) error

// defaultGoGetTimeout is the duration that `go get -u` is allowed
// to run, on average, per package.
const defaultGoGetTimeout = 30 * time.Second
