package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/cover"
)

var covDir = flag.String("covdir", "", "coverage directory")
var output = flag.String("output", "", "output file")
var usePackageName = flag.Bool("use-package-name", false, "use package name in output")
var sortByHits = flag.Bool("sort-by-hits", false, "sort by hits")
var printFuncs = flag.Bool("func", false, "summary by function")

func main() {
	flag.Parse()
	if *covDir == "" {
		log.Fatalf("covDir is empty")
	}
	info, err := os.Stat(*covDir)
	if err != nil || !info.IsDir() {
		log.Fatalf("covDir is not a directory")
	}
	log.Printf("using output file: %s", *output)
	res, err := getFileHitsPerTestCase(*covDir)
	if err != nil {
		log.Fatalf("error getting file hits: %v", err)
	}
	if *sortByHits {
		for _, v := range res {
			sort.Slice(v, func(i, j int) bool {
				return v[i].hit > v[j].hit
			})
		}
	}
	var writer = os.Stdout
	if *output != "" {
		writer, err = os.OpenFile(*output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			log.Fatalf("error creating output file: %v", err)
		}
		defer writer.Close()
	}
	testCaseNames := make([]string, 0, len(res))
	for k := range res {
		testCaseNames = append(testCaseNames, k)
	}
	sort.Strings(testCaseNames)
	fmt.Println(testCaseNames)
	for _, c := range testCaseNames {
		r := res[c]
		for _, f := range r {
			fmt.Fprintf(writer, "%s,%s,%d\n", c, f.file, f.hit)
		}
	}
	var printNames []string
	var touchedFiles []int
	for _, k := range testCaseNames {
		caseName, _ := strings.CutSuffix(k, "_test")
		printNames = append(printNames, caseName)
		counter := 0
		for _, f := range res[k] {
			if f.hit > 0 {
				counter++
			}
		}
		touchedFiles = append(touchedFiles, counter)
		fmt.Printf("%s:%v\n", caseName, counter)
	}
	for _, c := range printNames {
		fmt.Printf("%s,", c)
	}
	fmt.Println()
	for _, f := range touchedFiles {
		fmt.Printf("%d,", f)
	}
	fmt.Println()
	csv, _ := os.OpenFile("/tmp/sorted.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	defer csv.Close()
	for idx, c := range testCaseNames {
		hits := res[c][:5]
		for _, f := range hits {
			fName, _ := strings.CutPrefix(f.file, "antrea.io/antrea/")
			fmt.Fprintf(csv, "%s,%s,%d\n", printNames[idx], fName, f.hit)
		}
	}
}

func parseCovFile(file string) ([]fileHit, error) {
	profiles, err := cover.ParseProfiles(file)
	if err != nil {
		if strings.HasPrefix(err.Error(), "bad mode line") {
			return nil, nil
		}
		return nil, fmt.Errorf("error parsing profiles: %v", err)
	}
	if *printFuncs {
		return funcOutput(profiles)
	}
	fileHits := make([]fileHit, len(profiles))
	for idx, p := range profiles {
		fileHits[idx] = fileHit{
			file: p.FileName,
		}
	}
	for idx, p := range profiles {
		for _, b := range p.Blocks {
			fileHits[idx].hit += int64(b.NumStmt) * int64(b.Count)
		}
	}
	return fileHits, nil
}

type fileHit struct {
	file string
	hit  int64
}

func getFileHitsPerTestCase(dir string) (map[string][]fileHit, error) {
	res := make(map[string]map[string]int64)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Fatalf("error walking dir: %v", err)
		}
		if d.IsDir() {
			return nil
		}
		parsed, err := parseCovFile(path)
		if err != nil || parsed == nil {
			return err
		}
		log.Printf("parsed %s, %d records", path, len(parsed))
		testCaseName, _ := strings.CutPrefix(filepath.Dir(path), filepath.Dir(dir)+"/")
		if _, ok := res[testCaseName]; !ok {
			res[testCaseName] = make(map[string]int64)
		}
		for _, p := range parsed {
			filename := p.file
			if *usePackageName {
				filename = filepath.Dir(filename)
			}
			res[testCaseName][filename] += p.hit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	finalRes := make(map[string][]fileHit)
	for k, v := range res {
		for file, hit := range v {
			finalRes[k] = append(finalRes[k], fileHit{
				file: file,
				hit:  hit,
			})
		}
	}
	return finalRes, nil
}
