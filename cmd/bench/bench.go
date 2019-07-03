package main

import (
	"code.cloudfoundry.org/bytefmt"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/olekukonko/tablewriter"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	"github.com/spacemeshos/post/initialization"
	"github.com/spacemeshos/post/proving"
	"github.com/spacemeshos/post/shared"
	"github.com/spacemeshos/post/validation"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

type Config = shared.Config

var (
	id, _        = hex.DecodeString("deadbeef")
	challenge, _ = hex.DecodeString("this is a challenge")
	defConfig    = shared.DefaultConfig()
)

type benchMode int

const (
	single benchMode = 1 + iota
	mid
	full
)

var modes = []string{
	"single",
	"mid",
	"full",
}

func (m benchMode) isValid() bool { return m >= single && m <= full }

func (m benchMode) String() string { return modes[m-1] }

func main() {
	flag.StringVar(&defConfig.DataDir, "datadir", defConfig.DataDir, "filesystem datadir path")
	flag.Uint64Var(&defConfig.SpacePerUnit, "space", 1<<23, "space per unit, in bytes")
	flag.Uint64Var(&defConfig.FileSize, "filesize", 1<<23, "space per file, in bytes (in single mode only, otherwise it is autogenerated)")
	flag.UintVar(&defConfig.MaxWriteFilesParallelism, "pfiles", defConfig.MaxWriteFilesParallelism, "max degree of files write parallelism (in single mode only, otherwise it is autogenerated)")
	flag.UintVar(&defConfig.MaxWriteInFileParallelism, "pinfile", defConfig.MaxWriteInFileParallelism, "max degree of cpu work parallelism per file write (in single mode only, otherwise it is autogenerated)")
	flag.UintVar(&defConfig.MaxReadFilesParallelism, "pread", defConfig.MaxReadFilesParallelism, "max degree of files read parallelism (in single mode only, otherwise it is autogenerated)")
	mode := flag.Int("mode", int(mid), fmt.Sprintf("benchmark mode: %v=%d,%v=%d, %v=%d",
		single, single, mid, mid, full, full))
	disktype := flag.String("disktype", "", "specify the disk type (to be used in report)")
	fstype := flag.String("fstype", "", "specify the file-system type (to be used in report)")
	desc := flag.String("desc", "", "specify the test run description (to be used in report)")
	cpuprof := flag.String("cpuprof", "", "write cpu profile to file")
	memprof := flag.String("memprof", "", "write memory profile to file")
	report := flag.String("report", "report.csv", "write report csv to file")

	flag.Parse()

	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	benchMode := benchMode(*mode)
	if !benchMode.isValid() {
		log.Fatalf("invalid mode: %d", benchMode)
	}

	log.Printf("bench config: mode: %v, datadir: %v, space: %v",
		benchMode, defConfig.DataDir, bytefmt.ByteSize(defConfig.SpacePerUnit))

	cases := genTestCases(benchMode)
	data := make([][]string, 0)
	for i, cfg := range cases {
		log.Printf("test %v/%v starting...", i+1, len(cases))
		tStart := time.Now()

		init := initialization.NewInitializer(&cfg, shared.DisabledLogger{})
		prover := proving.NewProver(&cfg, shared.DisabledLogger{})
		validator := validation.NewValidator(&cfg)

		t := time.Now()
		proof, err := init.Initialize(id)
		if err != nil {
			log.Fatal(err)
		}
		eInit := time.Since(t)

		t = time.Now()
		err = validator.Validate(proof)
		if err != nil {
			log.Fatal(err)
		}
		eInitV := time.Since(t)

		t = time.Now()
		proof, err = prover.GenerateProof(id, challenge)
		if err != nil {
			log.Fatal(err)
		}
		eExec := time.Since(t)

		t = time.Now()
		err = validator.Validate(proof)
		if err != nil {
			log.Fatal(err)
		}
		eExecV := time.Since(t)

		err = init.Reset(id)
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("test %v/%v completed, %v", i+1, len(cases), time.Since(tStart))

		numFiles, _ := shared.NumFiles(cfg.SpacePerUnit, cfg.FileSize)
		pfiles, pinfile := init.CalcParallelism()
		pread := prover.CalcParallelism(numFiles)

		data = append(data, []string{
			strconv.Itoa(numFiles),
			strconv.Itoa(pfiles),
			strconv.Itoa(pinfile),
			eInit.Round(time.Duration(time.Millisecond)).String(),
			eInitV.Round(time.Duration(time.Microsecond)).String(),
			strconv.Itoa(pread),
			eExec.Round(time.Duration(time.Millisecond)).String(),
			eExecV.Round(time.Duration(time.Microsecond)).String(),
		})
	}

	header := []string{"NUMFILES", "P-FILES", "P-INFILE", "INIT", "INIT-V", "P-READ", "EXEC", "EXEC-V"}
	metadata := getMetadata(defConfig, *disktype, *fstype, *desc)

	exportTable(metadata, header, data, os.Stdout)
	exportCSV(metadata, header, data, *report)

	if *memprof != "" {
		f, err := os.Create(*memprof)
		if err != nil {
			log.Fatal("could not create memory profile: ", err)
		}
		defer f.Close()
		runtime.GC() // Get up-to-date statistics.
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
	}
}

func exportCSV(metadata []kv, header []string, data [][]string, path string) {
	file, err := os.Create(path)
	if err != nil {
		log.Panicf("report file creation failed: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Writing metadata.

	mdHeader := make([]string, len(metadata))
	mdData := make([]string, len(metadata))
	for i, item := range metadata {
		mdHeader[i] = item.k
		mdData[i] = item.v
	}

	if err := writer.WriteAll([][]string{mdHeader, mdData, {}}); err != nil {
		log.Panic(err)
	}

	// Writing data.

	if err := writer.Write(header); err != nil {
		log.Panic(err)
	}

	if err := writer.WriteAll(data); err != nil {
		log.Panic(err)
	}
}

func exportTable(metadata []kv, header []string, data [][]string, writer io.Writer) {
	fmt.Printf("\n- Results -\n")
	for _, item := range metadata {
		if item.k == "CPU_FLAGS" {
			continue
		}
		fmt.Printf("%v: %v\n", item.k, item.v)
	}

	table := tablewriter.NewWriter(writer)
	table.SetHeader(header)
	table.SetBorder(true)
	table.AppendBulk(data)
	table.Render()
}

func genTestCases(mode benchMode) []Config {
	cases := make([]Config, 0)
	if mode == single {
		cases = append(cases, *defConfig)
		return cases
	}

	def := *defConfig
	def.FileSize = defConfig.SpacePerUnit
	def.MaxWriteFilesParallelism = 1
	def.MaxWriteInFileParallelism = 1

	// Various in-file parallelism degrees.
	max := runtime.NumCPU()
	for i := 1; i <= max; i++ {
		cfg := def
		cfg.MaxWriteInFileParallelism = uint(i)

		switch mode {
		case full:
			cases = append(cases, cfg)
		case mid:
			if i == 1 || i == max {
				cases = append(cases, cfg)
			}
		}
	}

	// Split to files without files parallelism.
	max = 6
	for i := 1; i <= max; i++ {
		cfg := def
		cfg.FileSize >>= uint(i)

		switch mode {
		case full:
			cases = append(cases, cfg)
		case mid:
			if i == 1 || i == max {
				cases = append(cases, cfg)
			}
		}
	}

	// Split to files with max files parallelism degrees.
	max = 6
	for i := 1; i <= max; i++ {
		cfg := def
		cfg.FileSize >>= uint(i)
		cfg.MaxWriteFilesParallelism <<= uint(i)

		switch mode {
		case full:
			cases = append(cases, cfg)
		case mid:
			if i == 1 || i == max {
				cases = append(cases, cfg)
			}
		}
	}

	// Split to files with max files and in-file parallelism degrees.
	max = 4
	for i := 1; i <= max; i++ {
		cfg := def
		cfg.FileSize >>= uint(i)
		cfg.MaxWriteFilesParallelism <<= uint(i)
		cfg.MaxWriteInFileParallelism <<= uint(i)

		switch mode {
		case full:
			cases = append(cases, cfg)
		case mid:
			if i == 1 || i == 2 {
				cases = append(cases, cfg)
			}
		}
	}

	return cases
}

type kv struct {
	k string
	v string
}

func getMetadata(cfg *Config, disktype string, fstype string, desc string) []kv {
	// Using slice of kv instead of a map to maintain order.
	m := make([]kv, 0)

	if desc != "" {
		m = append(m, kv{k: "DESC", v: desc})

	}

	m = append(m, kv{k: "DATADIR", v: cfg.DataDir})
	m = append(m, kv{k: "SPACE", v: bytefmt.ByteSize(cfg.SpacePerUnit)})

	if disktype != "" {
		m = append(m, kv{k: "DISK", v: disktype})
	}

	if fstype != "" {
		m = append(m, kv{k: "FS", v: fstype})
	}

	m = append(m, kv{k: "OS", v: runtime.GOOS})

	cpuInfo, err := cpu.Info()
	if err != nil {
		log.Fatal(err)
	}
	m = append(m, kv{k: "CPU_MODEL", v: cpuInfo[0].ModelName})
	m = append(m, kv{k: "CPU_FLAGS", v: strings.Join(cpuInfo[0].Flags, " ")})
	m = append(m, kv{k: "CPU_CORES", v: strconv.Itoa(int(cpuInfo[0].Cores))})
	m = append(m, kv{k: "CPU_LOGICAL", v: strconv.Itoa(runtime.NumCPU())})

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		log.Fatal(err)
	}
	m = append(m, kv{k: "MEM_FREE", v: bytefmt.ByteSize(memInfo.Free)})

	return m
}
