package main

import (
	"fmt"
	"os"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"github.com/gunk/gunk/convert"
	"github.com/gunk/gunk/dump"
	"github.com/gunk/gunk/format"
	"github.com/gunk/gunk/generate"
	"github.com/gunk/gunk/log"
)

var (
	version = "v0.0.0-dev"

	app = kingpin.New("gunk", "The modern frontend and syntax for Protocol Buffers.").UsageTemplate(kingpin.CompactUsageTemplate)

	gen         = app.Command("generate", "Generate code from Gunk packages.")
	genPatterns = gen.Arg("patterns", "patterns of Gunk packages").Strings()

	conv                    = app.Command("convert", "Convert Proto file to Gunk file.")
	convProtoFilesOrFolders = conv.Arg("files_or_folders", "Proto files or folders to convert to Gunk").Strings()
	convOverwriteGunkFile   = conv.Flag("overwrite", "overwrite the converted Gunk file if it exists.").Bool()

	frmt         = app.Command("format", "Format Gunk code.")
	frmtPatterns = frmt.Arg("patterns", "patterns of Gunk packages").Strings()

	dmp         = app.Command("dump", "Write a FileDescriptorSet, defined in descriptor.proto")
	dmpPatterns = dmp.Arg("patterns", "patterns of Gunk packages").Strings()
	dmpFormat   = dmp.Flag("format", "output format: proto (default), or json").String()

	download     = app.Command("download", "Download required tools for Gunk, e.g., protoc")
	dlAll        = download.Command("all", "download all required tools")
	dlProtoc     = download.Command("protoc", "download protoc")
	dlProtocPath = dlProtoc.Flag("path", "path to check for protoc binary, or where to download it to").String()
	dlProtocVer  = dlProtoc.Flag("version", "version of protoc to use").String()

	ver = app.Command("version", "Show Gunk version.")
)

func main() {
	os.Exit(main1())
}

func main1() (code int) {
	// Replace kingpin's use of os.Exit, as testscript requires that we
	// return exit codes instead of exiting the entire program. Use a panic,
	// as we need to completely halt kingpin when it calls our terminate.
	type terminatedCode int
	app.Terminate(func(c int) { panic(terminatedCode(c)) })
	defer func() {
		r := recover()
		if c, ok := r.(terminatedCode); ok {
			code = int(c)
		} else if r != nil {
			panic(r)
		}
	}()
	app.HelpFlag.Short('h') // allow -h as well as --help

	gen.Flag("print-commands", "print the commands").Short('x').BoolVar(&log.PrintCommands)
	gen.Flag("verbose", "print the names of packages as they are generated").Short('v').BoolVar(&log.Verbose)

	download.Flag("verbose", "print details of downloaded tools").Short('v').BoolVar(&log.Verbose)
	downloadSubcommands := []func() error{
		downloadProtoc,
	}

	command, err := app.Parse(os.Args[1:])
	if code != 0 {
		// simulate the os.Exit that would have happened
		return code
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	switch command {
	case ver.FullCommand():
		fmt.Fprintf(os.Stdout, "gunk %s\n", version)
	case gen.FullCommand():
		err = generate.Run("", *genPatterns...)
	case conv.FullCommand():
		err = convert.Run(*convProtoFilesOrFolders, *convOverwriteGunkFile)
	case frmt.FullCommand():
		err = format.Run("", *frmtPatterns...)
	case dmp.FullCommand():
		err = dump.Run(*dmpFormat, "", *dmpPatterns...)
	case dlAll.FullCommand():
		for _, dl := range downloadSubcommands {
			err = dl()
			if err != nil {
				break
			}
		}
	case dlProtoc.FullCommand():
		err = downloadProtoc()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func downloadProtoc() error {
	_, err := generate.CheckOrDownloadProtoc(*dlProtocPath, *dlProtocVer)
	return err
}
