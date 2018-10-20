package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aykevl/go-llvm"
	"github.com/aykevl/tinygo/compiler"
)

var commands = map[string]string{
	"ar":    "ar",
	"clang": "clang-7",
}

// Helper function for Compiler object.
func Compile(pkgName, outpath, opt string, spec *TargetSpec, printIR, dumpSSA, debug bool, printSizes string, action func(string) error) error {
	config := compiler.Config{
		Triple:    spec.Triple,
		Debug:     debug,
		DumpSSA:   dumpSSA,
		RootDir:   sourceDir(),
		GOPATH:    getGopath(),
		BuildTags: spec.BuildTags,
	}
	c, err := compiler.NewCompiler(pkgName, config)
	if err != nil {
		return err
	}

	// Compile Go code to IR.
	parseErr := func() error {
		if printIR {
			// Run this even if c.Compile() panics.
			defer func() {
				fmt.Println("Generated LLVM IR:")
				fmt.Println(c.IR())
			}()
		}
		return c.Compile(pkgName)
	}()
	if parseErr != nil {
		return parseErr
	}

	c.ApplyFunctionSections() // -ffunction-sections

	if err := c.Verify(); err != nil {
		return err
	}

	// Optimization levels here are roughly the same as Clang, but probably not
	// exactly.
	switch opt {
	case "none:", "0":
		c.Optimize(0, 0, 0) // -O0
	case "1":
		c.Optimize(1, 0, 0) // -O1
	case "2":
		c.Optimize(2, 0, 225) // -O2
	case "s":
		c.Optimize(2, 1, 225) // -Os
	case "z":
		c.Optimize(2, 2, 5) // -Oz, default
	default:
		return errors.New("unknown optimization level: -opt=" + opt)
	}
	if err := c.Verify(); err != nil {
		return err
	}

	// On the AVR, pointers can point either to flash or to RAM, but we don't
	// know. As a temporary fix, load all global variables in RAM.
	// In the future, there should be a compiler pass that determines which
	// pointers are flash and which are in RAM so that pointers can have a
	// correct address space parameter (address space 1 is for flash).
	if strings.HasPrefix(spec.Triple, "avr") {
		c.NonConstGlobals()
		if err := c.Verify(); err != nil {
			return err
		}
	}

	// Generate output.
	outext := filepath.Ext(outpath)
	switch outext {
	case ".o":
		return c.EmitObject(outpath)
	case ".bc":
		return c.EmitBitcode(outpath)
	case ".ll":
		return c.EmitText(outpath)
	default:
		// Act as a compiler driver.

		// Create a temporary directory for intermediary files.
		dir, err := ioutil.TempDir("", "tinygo")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)

		// Write the object file.
		objfile := filepath.Join(dir, "main.o")
		err = c.EmitObject(objfile)
		if err != nil {
			return err
		}

		// Load builtins library from the cache, possibly compiling it on the
		// fly.
		var cachePath string
		if spec.CompilerRT {
			librt, err := loadBuiltins(spec.Triple)
			if err != nil {
				return err
			}
			cachePath, _ = filepath.Split(librt)
		}

		// Link the object file with the system compiler.
		executable := filepath.Join(dir, "main")
		tmppath := executable // final file
		args := append(spec.PreLinkArgs, "-o", executable, objfile)
		if spec.CompilerRT {
			args = append(args, "-L", cachePath, "-lrt-"+spec.Triple)
		}
		cmd := exec.Command(spec.Linker, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = sourceDir()
		err = cmd.Run()
		if err != nil {
			return err
		}

		if printSizes == "short" || printSizes == "full" {
			sizes, err := Sizes(executable)
			if err != nil {
				return err
			}
			if printSizes == "short" {
				fmt.Printf("   code    data     bss |   flash     ram\n")
				fmt.Printf("%7d %7d %7d | %7d %7d\n", sizes.Code, sizes.Data, sizes.BSS, sizes.Code+sizes.Data, sizes.Data+sizes.BSS)
			} else {
				fmt.Printf("   code  rodata    data     bss |   flash     ram | package\n")
				for _, name := range sizes.SortedPackageNames() {
					pkgSize := sizes.Packages[name]
					fmt.Printf("%7d %7d %7d %7d | %7d %7d | %s\n", pkgSize.Code, pkgSize.ROData, pkgSize.Data, pkgSize.BSS, pkgSize.Flash(), pkgSize.RAM(), name)
				}
				fmt.Printf("%7d %7d %7d %7d | %7d %7d | (sum)\n", sizes.Sum.Code, sizes.Sum.ROData, sizes.Sum.Data, sizes.Sum.BSS, sizes.Sum.Flash(), sizes.Sum.RAM())
				fmt.Printf("%7d       - %7d %7d | %7d %7d | (all)\n", sizes.Code, sizes.Data, sizes.BSS, sizes.Code+sizes.Data, sizes.Data+sizes.BSS)
			}
		}

		if outext == ".hex" || outext == ".bin" {
			// Get an Intel .hex file or .bin file from the .elf file.
			tmppath = filepath.Join(dir, "main"+outext)
			format := map[string]string{
				".hex": "ihex",
				".bin": "binary",
			}[outext]
			cmd := exec.Command(spec.Objcopy, "-O", format, executable, tmppath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				return err
			}
		}
		return action(tmppath)
	}
}

func Build(pkgName, outpath, target, opt string, printIR, dumpSSA, debug bool, printSizes string) error {
	spec, err := LoadTarget(target)
	if err != nil {
		return err
	}

	return Compile(pkgName, outpath, opt, spec, printIR, dumpSSA, debug, printSizes, func(tmppath string) error {
		if err := os.Rename(tmppath, outpath); err != nil {
			// Moving failed. Do a file copy.
			inf, err := os.Open(tmppath)
			if err != nil {
				return err
			}
			defer inf.Close()
			outf, err := os.OpenFile(outpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0777)
			if err != nil {
				return err
			}

			// Copy data to output file.
			_, err = io.Copy(outf, inf)
			if err != nil {
				return err
			}

			// Check whether file writing was successful.
			return outf.Close()
		} else {
			// Move was successful.
			return nil
		}
	})
}

func Flash(pkgName, target, opt, port string, printIR, dumpSSA, debug bool, printSizes string) error {
	spec, err := LoadTarget(target)
	if err != nil {
		return err
	}

	return Compile(pkgName, ".hex", opt, spec, printIR, dumpSSA, debug, printSizes, func(tmppath string) error {
		if spec.Flasher == "" {
			return errors.New("no flash command specified - did you miss a -target flag?")
		}

		// Create the command.
		flashCmd := spec.Flasher
		flashCmd = strings.Replace(flashCmd, "{hex}", tmppath, -1)
		flashCmd = strings.Replace(flashCmd, "{port}", port, -1)

		// Execute the command.
		cmd := exec.Command("/bin/sh", "-c", flashCmd)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = sourceDir()
		return cmd.Run()
	})
}

// Flash a program on a microcontroller and drop into a GDB shell.
//
// Note: this command is expected to execute just before exiting, as it
// modifies global state.
func FlashGDB(pkgName, target, opt, port string, printIR, dumpSSA, ocdOutput bool, printSizes string) error {
	spec, err := LoadTarget(target)
	if err != nil {
		return err
	}

	if spec.GDB == "" {
		return errors.New("gdb not configured in the target specification")
	}

	debug := true // always enable debug symbols
	return Compile(pkgName, "", opt, spec, printIR, dumpSSA, debug, printSizes, func(tmppath string) error {
		if len(spec.OCDDaemon) != 0 {
			// We need a separate debugging daemon for on-chip debugging.
			daemon := exec.Command(spec.OCDDaemon[0], spec.OCDDaemon[1:]...)
			if ocdOutput {
				// Make it clear which output is from the daemon.
				w := &ColorWriter{
					Out:    os.Stderr,
					Prefix: spec.OCDDaemon[0] + ": ",
					Color:  TermColorYellow,
				}
				daemon.Stdout = w
				daemon.Stderr = w
			}
			// Make sure the daemon doesn't receive Ctrl-C that is intended for
			// GDB (to break the currently executing program).
			// https://stackoverflow.com/a/35435038/559350
			daemon.SysProcAttr = &syscall.SysProcAttr{
				Setpgid: true,
				Pgid:    0,
			}
			// Start now, and kill it on exit.
			daemon.Start()
			defer func() {
				daemon.Process.Signal(os.Interrupt)
				// Maybe we should send a .Kill() after x seconds?
				daemon.Wait()
			}()
		}

		// Ignore Ctrl-C, it must be passed on to GDB.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func() {
			for range c {
			}
		}()

		// Construct and execute a gdb command.
		// By default: gdb -ex run <binary>
		// Exit GDB with Ctrl-D.
		params := []string{tmppath}
		for _, cmd := range spec.GDBCmds {
			params = append(params, "-ex", cmd)
		}
		cmd := exec.Command(spec.GDB, params...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
}

// Run the specified package directly (using JIT or interpretation).
func Run(pkgName string) error {
	config := compiler.Config{
		RootDir: sourceDir(),
		GOPATH:  getGopath(),
	}
	c, err := compiler.NewCompiler(pkgName, config)
	if err != nil {
		return errors.New("compiler: " + err.Error())
	}
	err = c.Compile(pkgName)
	if err != nil {
		return errors.New("compiler: " + err.Error())
	}
	if err := c.Verify(); err != nil {
		return errors.New("compiler error: failed to verify module: " + err.Error())
	}
	// -Oz, which is the fastest optimization level (faster than -O0, -O1, -O2
	// and -Os). Turn off the inliner, as the inliner increases optimization
	// time.
	c.Optimize(2, 2, 0)

	engine, err := llvm.NewExecutionEngine(c.Module())
	if err != nil {
		return errors.New("interpreter setup: " + err.Error())
	}
	defer engine.Dispose()

	main := engine.FindFunction("main")
	if main.IsNil() {
		return errors.New("could not find main function")
	}
	engine.RunFunction(main, nil)

	return nil
}

// Compile and run the given program in an emulator.
func Emulate(pkgName, target, opt string) error {
	spec, err := LoadTarget(target)
	if err != nil {
		return err
	}
	if len(spec.Emulator) == 0 {
		return errors.New("no emulator configured for this target")
	}

	return Compile(pkgName, ".elf", opt, spec, false, false, false, "", func(tmppath string) error {
		args := append(spec.Emulator[1:], tmppath)
		cmd := exec.Command(spec.Emulator[0], args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			if err, ok := err.(*exec.ExitError); ok && err.Exited() {
				// Workaround for QEMU which always exits with an error.
				return nil
			}
		}
		return err
	})
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s command [-printir] [-target=<target>] -o <output> <input>\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "\ncommands:")
	fmt.Fprintln(os.Stderr, "  build: compile packages and dependencies")
	fmt.Fprintln(os.Stderr, "  flash: compile and flash to the device")
	fmt.Fprintln(os.Stderr, "  help:  print this help text")
	fmt.Fprintln(os.Stderr, "  run:   run package in an interpreter")
	fmt.Fprintln(os.Stderr, "\nflags:")
	flag.PrintDefaults()
}

func main() {
	outpath := flag.String("o", "", "output filename")
	opt := flag.String("opt", "z", "optimization level: 0, 1, 2, s, z")
	printIR := flag.Bool("printir", false, "print LLVM IR")
	dumpSSA := flag.Bool("dumpssa", false, "dump internal Go SSA")
	target := flag.String("target", "", "LLVM target")
	printSize := flag.String("size", "", "print sizes (none, short, full)")
	nodebug := flag.Bool("no-debug", false, "disable DWARF debug symbol generation")
	ocdOutput := flag.Bool("ocd-output", false, "print OCD daemon output during debug")
	port := flag.String("port", "/dev/ttyACM0", "flash port")

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "No command-line arguments supplied.")
		usage()
		os.Exit(1)
	}
	command := os.Args[1]

	flag.CommandLine.Parse(os.Args[2:])

	os.Setenv("CC", "clang -target="+*target)

	switch command {
	case "build":
		if *outpath == "" {
			fmt.Fprintln(os.Stderr, "No output filename supplied (-o).")
			usage()
			os.Exit(1)
		}
		if flag.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "No package specified.")
			usage()
			os.Exit(1)
		}
		target := *target
		if target == "" && filepath.Ext(*outpath) == ".wasm" {
			target = "wasm"
		}
		err := Build(flag.Arg(0), *outpath, target, *opt, *printIR, *dumpSSA, !*nodebug, *printSize)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "flash", "gdb":
		if *outpath != "" {
			fmt.Fprintln(os.Stderr, "Output cannot be specified with the flash command.")
			usage()
			os.Exit(1)
		}
		var err error
		if command == "flash" {
			err = Flash(flag.Arg(0), *target, *opt, *port, *printIR, *dumpSSA, !*nodebug, *printSize)
		} else {
			err = FlashGDB(flag.Arg(0), *target, *opt, *port, *printIR, *dumpSSA, *ocdOutput, *printSize)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "run":
		if flag.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "No package specified.")
			usage()
			os.Exit(1)
		}
		var err error
		if *target == "" {
			err = Run(flag.Arg(0))
		} else {
			err = Emulate(flag.Arg(0), *target, *opt)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "clean":
		// remove cache directory
		dir := cacheDir()
		err := os.RemoveAll(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot clean cache:", err)
			os.Exit(1)
		}
	case "help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "Unknown command:", command)
		usage()
		os.Exit(1)
	}
}
