// Command poolbench is a CPU-bound task for measuring how much a pool speeds
// things up. Each run counts primes below a limit by trial division on one
// core, prints a line, and writes it to out/result-TASK.txt.
//
//	pool map --range 1..48 --ship poolbench.exe --collect out -- poolbench.exe {}
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func countPrimes(limit int) int {
	if limit < 3 {
		return 0
	}
	n := 1 // 2
	for c := 3; c < limit; c += 2 {
		prime := true
		for d := 3; d*d <= c; d += 2 {
			if c%d == 0 {
				prime = false
				break
			}
		}
		if prime {
			n++
		}
	}
	return n
}

func main() {
	limit := flag.Int("n", 3_000_000, "count primes below this number")
	flag.Parse()
	task := flag.Arg(0)
	if task == "" {
		task = "0"
	}
	start := time.Now()
	primes := countPrimes(*limit)
	host, _ := os.Hostname()
	line := fmt.Sprintf("task %s: %d primes below %d in %.2fs on %s\n", task, primes, *limit, time.Since(start).Seconds(), host)
	fmt.Print(line)
	if err := os.MkdirAll("out", 0o755); err == nil {
		_ = os.WriteFile(filepath.Join("out", "result-"+task+".txt"), []byte(line), 0o644)
	}
}
