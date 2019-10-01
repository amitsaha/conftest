package test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/instrumenta/conftest/pkg/commands/update"
	"github.com/instrumenta/conftest/pkg/constants"
	"github.com/instrumenta/conftest/pkg/parser"

	"github.com/containerd/containerd/log"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/topdown"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	DenyQ                 = regexp.MustCompile("^(deny|violation)(_[a-zA-Z]+)*$")
	WarnQ                 = regexp.MustCompile("^warn(_[a-zA-Z]+)*$")
	CombineConfigFlagName = "combine-config"
)

// CheckResult describes the result of a conftest evaluation.
// warning and failure "errors" produced by rego should be considered separate
// from other classes of exceptions.
type CheckResult struct {
	Warnings []error
	Failures []error
}

// NewTestCommand creates a new test command
func NewTestCommand(osExit func(int), getOutputManager func() OutputManager) *cobra.Command {

	ctx := context.Background()
	cmd := &cobra.Command{
		Use:     "test <file> [file...]",
		Short:   "Test your configuration files using Open Policy Agent",
		Version: fmt.Sprintf("Version: %s\nCommit: %s\nDate: %s\n", constants.Version, constants.Commit, constants.Date),

		Run: func(cmd *cobra.Command, fileList []string) {
			out := getOutputManager()
			if len(fileList) < 1 {
				cmd.SilenceErrors = true
				log.G(ctx).Fatal("The first argument should be a file")
			}

			if viper.GetBool("update") {
				update.NewUpdateCommand().Run(cmd, fileList)
			}

			compiler, err := buildCompiler(viper.GetString("policy"))
			if err != nil {
				log.G(ctx).Fatalf("Problem building rego compiler: %s", err)
			}
			foundFailures := false
			var configFiles []parser.ConfigDoc
			var fileType string
			for _, fileName := range fileList {
				var err error
				var config io.ReadCloser
				fileType, err = getFileType(viper.GetString("input"), fileName)
				if err != nil {
					log.G(ctx).Errorf("Unable to get file type: %v", err)
					osExit(1)
				}
				config, err = getConfig(fileName)
				if err != nil {
					log.G(ctx).Errorf("Unable to open file or read from stdin %s", err)
					osExit(1)
				}
				configFiles = append(configFiles, parser.ConfigDoc{
					ReadCloser: config,
					Filepath:   fileName,
				})
			}
			configManager := parser.NewConfigManager(fileType)
			configurations, err := configManager.BulkUnmarshal(configFiles)
			if err != nil {
				log.G(ctx).Errorf("Unable to BulkUnmarshal your config files: %v", err)
				osExit(1)
			}
			for k, v := range configurations {
				fmt.Printf("Top-level Key %#v\n", k)
				fmt.Printf("Values: %#v\n", v)
			}

			var res CheckResult
			if viper.GetBool(CombineConfigFlagName) {
				res, err = processData(ctx, configurations, compiler)
				if err != nil {
					log.G(ctx).Fatalf("Problem processing data: %s", err)
				}
				err = out.Put("Combined-configs (multi-file)", res)
				if err != nil {
					log.G(ctx).Fatalf("Problem generating output: %s", err)
				}
			} else {
				for fileName, config := range configurations {
					res, err = processData(ctx, config, compiler)
					if err != nil {
						log.G(ctx).Fatalf("Problem processing data: %s", err)
					}
					err = out.Put(fileName, res)
					if err != nil {
						log.G(ctx).Fatalf("Problem generating output: %s", err)
					}
				}
			}
			if len(res.Failures) > 0 || (len(res.Warnings) > 0 && viper.GetBool("fail-on-warn")) {
				foundFailures = true
			}

			err = out.Flush()
			if err != nil {
				log.G(ctx).Fatal(err)
			}

			if foundFailures {
				osExit(1)
			}
		},
	}

	cmd.Flags().BoolP("fail-on-warn", "", false, "return a non-zero exit code if only warnings are found")
	cmd.Flags().BoolP("update", "", false, "update any policies before running the tests")
	cmd.Flags().BoolP(CombineConfigFlagName, "", false, "combine all given config files to be evaluated together")

	cmd.Flags().StringP("output", "o", "", fmt.Sprintf("output format for conftest results - valid options are: %s", validOutputs()))
	cmd.Flags().StringP("input", "i", "", fmt.Sprintf("input type for given source, especially useful when using conftest with stdin, valid options are: %s", parser.ValidInputs()))

	var err error
	flagNames := []string{"fail-on-warn", "update", CombineConfigFlagName, "output", "input"}
	for _, name := range flagNames {
		err = viper.BindPFlag(name, cmd.Flags().Lookup(name))
		if err != nil {
			log.G(ctx).Fatal("Failed to bind argument:", err)
		}
	}

	return cmd
}

func getConfig(fileName string) (io.ReadCloser, error) {
	if fileName == "-" {
		config := ioutil.NopCloser(bufio.NewReader(os.Stdin))
		return config, nil
	}

	filePath, _ := filepath.Abs(fileName)
	config, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("Unable to open file %s: %s", filePath, err)
	}
	return config, nil
}

func buildRego(trace bool, query string, input interface{}, compiler *ast.Compiler) (*rego.Rego, *topdown.BufferTracer) {
	var regoObj *rego.Rego
	var regoFunc []func(r *rego.Rego)
	buf := topdown.NewBufferTracer()

	regoFunc = append(regoFunc, rego.Query(query), rego.Compiler(compiler), rego.Input(input))
	if trace {
		regoFunc = append(regoFunc, rego.Tracer(buf))
	}
	regoObj = rego.New(regoFunc...)

	return regoObj, buf
}

func getFileType(inputFileType, fileName string) (string, error) {
	if inputFileType != "" {
		return inputFileType, nil
	}
	if fileName == "-" && inputFileType == "" {
		return "yaml", nil
	}
	if fileName != "-" {
		fileType := ""
		if strings.Contains(fileName, ".") {
			fileType = strings.TrimPrefix(filepath.Ext(fileName), ".")
		} else {
			ss := strings.SplitAfter(fileName, "/")
			fileType = ss[len(ss)-1]
		}

		return fileType, nil
	}
	return "", fmt.Errorf("not supported filetype")
}

// finds all queries in the compiler
func getRules(ctx context.Context, re *regexp.Regexp, compiler *ast.Compiler) []string {

	var res []string

	for _, m := range compiler.Modules {
		for _, r := range m.Rules {
			n := r.Head.Name.String()
			if re.MatchString(n) {
				// the same rule names can be used multiple times, but
				// we only want to run the query and report results once
				if !stringInSlice(n, res) {
					res = append(res, n)
				}
			}
		}
	}
	return res
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func makeQuery(rule string) string {
	return fmt.Sprintf("data.%s.%s", viper.GetString("namespace"), rule)
}

func processData(ctx context.Context, input interface{}, compiler *ast.Compiler) (CheckResult, error) {
	// collect warnings
	var warnings []error
	for _, rule := range getRules(ctx, WarnQ, compiler) {
		warns, err := runQuery(ctx, makeQuery(rule), input, compiler)
		if err != nil {
			return CheckResult{}, err
		}

		warnings = append(warnings, warns...)
	}

	// collect failures
	var failures []error
	for _, r := range getRules(ctx, DenyQ, compiler) {
		fails, err := runQuery(ctx, makeQuery(r), input, compiler)
		if err != nil {
			return CheckResult{}, err
		}
		failures = append(failures, fails...)
	}

	return CheckResult{
		Failures: failures,
		Warnings: warnings,
	}, nil
}

func runQuery(ctx context.Context, query string, input interface{}, compiler *ast.Compiler) ([]error, error) {
	fmt.Printf("Running query with %#v\n", input)
	hasResults := func(expression interface{}) bool {
		if v, ok := expression.([]interface{}); ok {
			return len(v) > 0
		}
		return false
	}

	r, stdout := buildRego(viper.GetBool("trace"), query, input, compiler)
	rs, err := r.Eval(ctx)

	if err != nil {
		return nil, fmt.Errorf("Problem evaluating r policy: %s", err)
	}

	topdown.PrettyTrace(os.Stdout, *stdout)

	var errs []error

	for _, r := range rs {
		for _, e := range r.Expressions {
			value := e.Value
			if hasResults(value) {
				for _, v := range value.([]interface{}) {
					errs = append(errs, errors.New(v.(string)))
				}
			}
		}
	}

	return errs, nil
}

func buildCompiler(path string) (*ast.Compiler, error) {
	files, err := recursivelySearchDirForRegoFiles(path)
	if err != nil {
		return nil, err
	}

	modules := map[string]*ast.Module{}

	for _, file := range files {
		out, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}

		name := filepath.Base(file)
		parsed, err := ast.ParseModule(name, string(out[:]))
		if err != nil {
			return nil, err
		}
		modules[name] = parsed
	}

	compiler := ast.NewCompiler()
	compiler.Compile(modules)

	if compiler.Failed() {
		return nil, compiler.Errors
	}

	return compiler, nil
}

func recursivelySearchDirForRegoFiles(path string) ([]string, error) {
	var filepaths []string
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), ".rego") {
			filepaths = append(filepaths, path)
		}

		return nil
	})

	return filepaths, err
}
