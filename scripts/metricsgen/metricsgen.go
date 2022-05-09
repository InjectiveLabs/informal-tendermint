// metricsgen is a code generation tool for creating constructors for Tendermint
// metrics types.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"text/template"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s -dir <dir> -struct <struct>

Generate constructors for the metrics type contained in the specified -dir
Go directory. The tool creates a new file in the same directory as the specified directory
containing the generated code.

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

const (
	metricNameTag = "metricsgen_name"
	labelsTag     = "metricsgen_labels"
	bucketTypeTag = "metricsgen_bucketsType"
	bucketSizeTag = "metricsgen_bucketSizes"
)

var (
	dir   = flag.String("dir", ".", "Path to the directory containing the target package")
	strct = flag.String("struct", "Metrics", "Struct to parse for metrics")
)

var bucketType = map[string]string{
	"exprange": "stdprometheus.ExponentialBucketsRange",
	"exp":      "stdprometheus.ExponentialBuckets",
	"lin":      "stdprometheus.LinearBuckets",
}

var tmpl = template.Must(template.New("tmpl").Parse(`// Code generated by metricsgen. DO NOT EDIT.

package {{ .Package }}

import (
	"github.com/go-kit/kit/metrics/discard"
	prometheus "github.com/go-kit/kit/metrics/prometheus"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
)

func PrometheusMetrics(namespace string, labelsAndValues...string) *Metrics {
	labels := []string{}
	for i := 0; i < len(labelsAndValues); i += 2 {
		labels = append(labels, labelsAndValues[i])
	}
	return &Metrics{
		{{ range $metric := .ParsedMetrics }}
		{{- $metric.FieldName }}: prometheus.New{{ $metric.TypeName }}From(stdprometheus.{{$metric.TypeName }}Opts{
			Namespace: namespace,
			Subsystem: MetricsSubsystem,
			Name:      "{{$metric.MetricName }}",
			Help:      "{{ $metric.Description }}",
			{{ if ne $metric.HistogramOptions.BucketType "" }}
			Buckets: {{ $metric.HistogramOptions.BucketType }}({{ $metric.HistogramOptions.BucketSizes }}),
			{{ else if ne $metric.HistogramOptions.BucketSizes "" }}
			Buckets: []float64{ {{ $metric.HistogramOptions.BucketSizes }} },
			{{ end }}
		{{- if eq (len $metric.Labels) 0 }}
		}, labels).With(labelsAndValues...),
		{{ else }}
		}, append(labels, {{$metric.Labels | printf "%q" }})).With(labelsAndValues...),
		{{- end }}
		{{- end }}
	}
}


func NopMetrics() *Metrics {
	return &Metrics{
		{{- range $metric := .ParsedMetrics }}
		{{ $metric.FieldName }}: discard.New{{ $metric.TypeName }}(),
		{{- end }}
	}
}
`))

// ParsedMetricField is the data parsed for a single field of a metric struct.
type ParsedMetricField struct {
	TypeName    string
	FieldName   string
	MetricName  string
	Description string
	Labels      string

	HistogramOptions HistogramOpts
}

type HistogramOpts struct {
	BucketType  string
	BucketSizes string
}

// TemplateData is all of the data required for rendering a metric file template.
type TemplateData struct {
	Package       string
	ParsedMetrics []ParsedMetricField
}

func main() {
	flag.Parse()
	if *strct == "" {
		log.Fatal("You must specify a non-empty -struct")
	}
	if *dir == "" {
		log.Fatal("You must specify a non-empty -dir")
	}
	td, err := ParseMetricsDir(*dir, *strct)
	if err != nil {
		log.Fatalf("Parsing file: %v", err)
	}
	out := filepath.Join(*dir, "metrics.gen.go")
	f, err := os.Create(out)
	if err != nil {
		log.Fatalf("Opening file: %v", err)
	}
	err = GenerateMetricsFile(f, td)
	if err != nil {
		log.Fatalf("Generating code: %v", err)
	}
}
func ignoreTestFiles(f fs.FileInfo) bool {
	return !strings.Contains(f.Name(), "_test.go")
}

// ParseMetricsDir parses the dir and scans for a struct matching structName,
// ignoring all test files. ParseMetricsDir iterates the fields of the metrics
// struct and builds a TemplateData using the data obtained from the abstract syntax tree.
func ParseMetricsDir(dir string, structName string) (TemplateData, error) {
	fs := token.NewFileSet()
	d, err := parser.ParseDir(fs, dir, ignoreTestFiles, parser.ParseComments)
	if err != nil {
		return TemplateData{}, err
	}
	if len(d) > 1 {
		return TemplateData{}, fmt.Errorf("multiple packages found in %s", dir)
	}
	if len(d) == 0 {
		return TemplateData{}, fmt.Errorf("no go pacakges found in %s", dir)
	}

	// Grab the package name.
	var pkgName string
	var pkg *ast.Package
	for pkgName, pkg = range d {
	}
	td := TemplateData{
		Package: pkgName,
	}
	// Grab the metrics struct
	m, err := fetchMetricsStruct(pkg.Files, structName)
	if err != nil {
		return TemplateData{}, err
	}
	for _, f := range m.Fields.List {
		if !isMetric(f.Type) {
			continue
		}
		pmf := parseMetricField(f)
		td.ParsedMetrics = append(td.ParsedMetrics, pmf)
	}

	return td, err
}

// GenerateMetricsFile executes the metrics file template, writing the result
// into the io.Writer.
func GenerateMetricsFile(w io.Writer, td TemplateData) error {
	b := []byte{}
	buf := bytes.NewBuffer(b)
	err := tmpl.Execute(buf, td)
	if err != nil {
		return err
	}
	b, err = format.Source(buf.Bytes())
	if err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	return nil
}

func fetchMetricsStruct(files map[string]*ast.File, structName string) (*ast.StructType, error) {
	var (
		err error
		st  *ast.StructType
	)
	for _, file := range files {
		if !ast.FilterFile(file, func(name string) bool {
			return name == structName
		}) {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch f := n.(type) {
			case *ast.TypeSpec:
				if f.Name.Name == structName {
					var ok bool
					st, ok = f.Type.(*ast.StructType)
					if !ok {
						err = fmt.Errorf("found identifier for %q of wrong type", structName)
					}
				}
				return false
			default:
				return true
			}
		})
		if err != nil {
			return nil, err
		}
		if st != nil {
			return st, nil
		}
	}
	return nil, fmt.Errorf("target struct %q not found in dir", structName)
}

func parseMetricField(f *ast.Field) ParsedMetricField {
	var comment string
	if f.Doc != nil {
		for _, c := range f.Doc.List {
			comment += strings.TrimPrefix(c.Text, "// ")
		}
	}
	pmf := ParsedMetricField{
		Description: comment,
		MetricName:  extractFieldName(f.Names[0].String(), f.Tag),
		FieldName:   f.Names[0].String(),
		TypeName:    extractTypeName(f.Type),
		Labels:      extractLabels(f.Tag),
	}
	if pmf.TypeName == "Histogram" {
		pmf.HistogramOptions = extractHistogramOptions(f.Tag)
	}
	return pmf
}

func extractTypeName(e ast.Expr) string {
	return strings.TrimPrefix(types.ExprString(e), "metrics.")
}

func isMetric(e ast.Expr) bool {
	return strings.Contains(types.ExprString(e), "metrics.")
}

func extractLabels(bl *ast.BasicLit) string {
	if bl != nil {
		t := reflect.StructTag(strings.Trim(bl.Value, "`"))
		if v := t.Get(labelsTag); v != "" {
			return v
		}
	}
	return ""
}

func extractFieldName(name string, tag *ast.BasicLit) string {
	if tag != nil {
		t := reflect.StructTag(strings.Trim(tag.Value, "`"))
		if v := t.Get(metricNameTag); v != "" {
			return v
		}
	}
	return toSnakeCase(name)
}

func extractHistogramOptions(tag *ast.BasicLit) HistogramOpts {
	h := HistogramOpts{}
	if tag != nil {
		t := reflect.StructTag(strings.Trim(tag.Value, "`"))
		if v := t.Get(bucketTypeTag); v != "" {
			h.BucketType = bucketType[v]
		}
		if v := t.Get(bucketSizeTag); v != "" {
			h.BucketSizes = v
		}
	}
	return h
}

var capitalChange = regexp.MustCompile("([a-z0-9])([A-Z])")

func toSnakeCase(str string) string {
	snake := capitalChange.ReplaceAllString(str, "${1}_${2}")
	return strings.ToLower(snake)
}