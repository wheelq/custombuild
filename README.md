custombuild
============

A fairly simple package that makes it easier to generate custom builds of your Go programs. *This package is still very experimental.*

It takes as input three things:

- Path to the repository which will be customized
- A function that performs code generation to customize the build
- List of packages that are new dependencies of the modified code base


### Example

```go
// Prepare a custom build that uses two new packages the original project doesn't use
builder, err := custombuild.New("../myproject", codeGenFunc, []string{
	"github.com/fizz/fizz",
	"github.com/foo/foo",
})
defer builder.Teardown() // always do this, even if error
if err != nil {
	log.Fatal(err)
}

// Build for 64-bit Windows
err = builder.Build("windows", "amd64", "./custom_build_windows")
if err != nil {
	log.Fatal(err)
}

// Build for ARMv6 Linux
err = builder.BuildARM("linux", 7, "./custom_build_arm")
if err != nil {
	log.Fatal(err)
}
```


### Requirements

This tool requires the go tool to be installed and in your PATH. Since this tool calls `go get -u`, it requires any necessary version control system to be installed also (usually git).

