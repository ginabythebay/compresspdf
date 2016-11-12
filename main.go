package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var (
	gs      = mustLocate("gs")
	pdfinfo = mustLocate("pdfinfo")
)

func mustLocate(pgm string) string {
	p, err := exec.LookPath(pgm)
	if err != nil {
		panic(err)
	}
	return p
}

func main() {
	app := cli.NewApp()
	app.Name = "compresspdf"
	app.Usage = "Compresses one or more pdf files in place, if they don't appear to have been processed already.  Requires that gs and pdfinfo are in the PATH"
	app.UsageText = "compresspdf [global options] <pdf files> [pdf files...]"
	app.HideVersion = true
	app.Action = compressAll
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "f, force",
			Usage: "Attempts compression even if the file may have already been compressed",
		},
		cli.BoolFlag{
			Name:  "q, quiet",
			Usage: "No output unless there is an error",
		},
		cli.BoolFlag{
			Name:  "v, verbose",
			Usage: "Extra output",
		},
	}
	err := app.Run(os.Args)
	if err != nil {
		fmt.Printf("%+v\n", err)
		os.Exit(1)
	}
}

func compressAll(c *cli.Context) (err error) {
	args := c.Args()
	if len(args) == 0 {
		return cli.NewExitError("You must specify the name of at least one pdf to compress", 1)
	}

	var workDir string
	workDir, err = ioutil.TempDir("", "compresspdf")
	if err != nil {
		return err
	}
	// defer func() {
	// 	rmvErr := os.RemoveAll(workDir)
	// 	if rmvErr != nil && err == nil {
	// 		err = rmvErr
	// 	}
	// }()

	comp := compressor{
		c.Bool("force"),
		c.Bool("verbose"),
		c.Bool("quiet"),
		args,
		workDir,
	}
	return comp.compressAll()
}

type compressor struct {
	// command line options
	optForce   bool
	optVerbose bool
	optQuiet   bool

	allTargets []string

	workDir string
}

func (c *compressor) compressAll() error {
	var cnt int
	for _, t := range c.allTargets {
		didCompress, err := c.maybeCompress(t)
		if err != nil {
			return err
		}
		if didCompress {
			cnt++
		}
	}
	if !c.optQuiet {
		if cnt == 1 {
			fmt.Printf("Compressed 1 file\n")
		} else {
			fmt.Printf("Compressed %d files\n", cnt)
		}
	}
	return nil
}

func (c *compressor) verbose(format string, a ...interface{}) {
	if !c.optVerbose {
		return
	}
	fmt.Println(fmt.Sprintf(format, a...))
}

func (c *compressor) maybeCompress(target string) (bool, error) {
	if !c.optForce {
		skip, err := c.appearsCompressed(target)
		if err != nil {
			return false, err
		}
		if skip {
			c.verbose("Skipping %s as it appears to be already-compressed", target)
			return false, nil
		}
	}
	return c.compress(target)
}

func (c *compressor) appearsCompressed(target string) (bool, error) {
	cmd := exec.Command(pdfinfo, target)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return false, errors.Wrapf(err, "Running %q returned %q", cmd.Args, string(b))
	}
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	for scanner.Scan() {
		line := scanner.Text()
		tokens := strings.SplitN(line, ":", 2)
		if len(tokens) != 2 {
			msg := fmt.Sprintf("Unexpected line of output %q in \n%s\n, when running %q", line, string(b), cmd.Args)
			return false, cli.NewExitError(msg, 1)
		}
		key := strings.TrimSpace(tokens[0])
		value := tokens[1]
		if key == "Producer" && strings.Contains(value, "Ghostscript") {
			return true, nil
		}
	}

	return false, nil
}

type argStruct struct {
	Setting string
	Input   string
	Output  string
}

var argTempl = template.Must(template.New("args").Parse(strings.TrimSpace(`
-dPDFSETTINGS=/{{.Setting}}
-sOutputFile={{.Output}}
-sDEVICE=pdfwrite
-dCompatibilityLevel=1.4
-dNOPAUSE
-dQUIET
-dBATCH
{{.Input}}
`)))

func (c *compressor) compress(target string) (bool, error) {
	tmpfile := path.Join(c.workDir, path.Base(target))

	var b bytes.Buffer
	err := argTempl.Execute(&b, argStruct{"screen", target, tmpfile})
	if err != nil {
		return false, err
	}
	args := strings.Split(b.String(), "\n")

	cmd := exec.Command(gs, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, errors.Wrapf(err, "Running %q returned %q", cmd.Args, string(out))
	}
	var oldFile, newFile os.FileInfo
	oldFile, err = os.Stat(target)
	if err != nil {
		return false, errors.Wrapf(err, "Stating old file %q", target)
	}
	newFile, err = os.Stat(tmpfile)
	if err != nil {
		return false, errors.Wrapf(err, "Stating new file %q", tmpfile)
	}

	growth := newFile.Size() - oldFile.Size()
	if growth > 0 {
		c.verbose("Compressing %q made it grow from %s by %s; skipping.", target, humanize(oldFile.Size()), humanize(growth))
		return false, nil
	}

	err = copyFile(tmpfile, target)
	if err != nil {
		return false, errors.Wrapf(err, "Rename from %q to %q", tmpfile, target)
	}
	pct := percent(oldFile.Size(), newFile.Size())
	c.verbose("Shrank %q to %s, (%%%s of its original size)", target, humanize(newFile.Size()), pct)

	return true, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return
	}
	err = out.Sync()
	return
}

var suffixes = []string{
	"b",
	"K",
	"M",
	"G",
}

func percent(old, new int64) string {
	f := 100.0 * float64(new) / float64(old)
	switch {
	case f < 1:
		return fmt.Sprintf("%.2f", f)
	case f < 10:
		return fmt.Sprintf("%.1f", f)
	default:
		return fmt.Sprintf("%.0f", f)
	}
}

func humanize(i int64) string {
	f := float64(i)
	s := suffixes[len(suffixes)-1]
	for _, candidate := range suffixes {
		if f < 1024 {
			s = candidate
			break
		}
		f = f / 1024
	}
	return fmt.Sprintf("%.1f%s", f, s)
}
