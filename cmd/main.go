// siliconctl/cmd/main.go
// CLI entry-point: status | watch | cpu | fan | export

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anoopjohn-dev/siliconctl/hardware"
)

const banner = `
  ██████ ██ ██      ██  ██████  ██████  ███    ██  ██████ ████████ ██
 ██      ██ ██      ██ ██      ██    ██ ████   ██ ██         ██    ██
  █████  ██ ██      ██ ██      ██    ██ ██ ██  ██ ██         ██    ██
      ██ ██ ██      ██ ██      ██    ██ ██  ██ ██ ██         ██    ██
 ██████  ██ ███████ ██  ██████  ██████  ██   ████  ██████    ██    ███████
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(banner)
		printUsage()
		os.Exit(0)
	}

	mon := hardware.New()

	switch os.Args[1] {
	case "status":
		cmdStatus(mon, false)
	case "watch":
		watchCmd := flag.NewFlagSet("watch", flag.ExitOnError)
		interval := watchCmd.Duration("interval", time.Second, "Polling interval")
		watchCmd.Parse(os.Args[2:])
		for {
			cmdStatus(mon, true)
			time.Sleep(*interval)
		}
	case "export":
		exportCmd := flag.NewFlagSet("export", flag.ExitOnError)
		format   := exportCmd.String("format", "json", "Output format: json")
		interval := exportCmd.Duration("interval", time.Second, "Emit interval")
		exportCmd.Parse(os.Args[2:])
		for {
			snap, err := mon.Snapshot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
			if *format == "json" {
				b, _ := json.Marshal(snap)
				fmt.Println(string(b))
			}
			time.Sleep(*interval)
		}
	case "cpu":
		cmdCPU(os.Args[2:])
	case "fan":
		cmdFan(os.Args[2:])
	case "help", "--help", "-h":
		fmt.Print(banner)
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: siliconctl <command> [flags]

Commands:
  status           Print a hardware snapshot
  watch            Continuously refresh the dashboard
    --interval     Polling interval  (default: 1s)
  export           Stream metrics as JSON to stdout
    --format       Output format: json  (default: json)
    --interval     Emit interval  (default: 1s)
  cpu governor <g> Set CPU scaling governor for all cores
                   Governors: performance powersave ondemand schedutil
  fan set <n> <v>  Set fan PWM percentage (0-100)

Examples:
  siliconctl status
  siliconctl watch --interval 500ms
  siliconctl cpu governor performance
  siliconctl fan set cpu_fan 75
  siliconctl export --format json | jq .memory`)
}

func cmdStatus(mon *hardware.Monitor, clear bool) {
	snap, err := mon.Snapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}

	if clear {
		fmt.Print("\033[H\033[2J") // ANSI clear
	}

	w := 58
	line := strings.Repeat("─", w)

	fmt.Printf("┌%s┐\n", line)
	fmt.Printf("│  siliconctl  │  %s  │  %s  │\n",
		time.Now().Format("15:04:05"),
		snap.Timestamp)
	fmt.Printf("├%s┤\n", line)

	// CPU
	fmt.Printf("│  %-8s  %-8s  %-10s  %-16s  │\n",
		"Core", "Freq", "Temp", "Usage")
	fmt.Printf("│  %s  │\n", strings.Repeat("─", w-4))
	for _, c := range snap.Cores {
		bar := usageBar(c.UsagePct, 16)
		temp := "  ---  "
		if c.TempC > 0 {
			temp = fmt.Sprintf("%5.1f°C", c.TempC)
		}
		fmt.Printf("│  Core %-2d  %4d MHz  %s  %s %3.0f%%  │\n",
			c.ID, c.FreqMHz, temp, bar, c.UsagePct)
	}

	// Memory
	m := snap.Memory
	fmt.Printf("├%s┤\n", line)
	fmt.Printf("│  Memory: %d / %d MB  %s  %d%%  │\n",
		m.UsedMB, m.TotalMB,
		usageBar(float64(m.PctUsed), 16), m.PctUsed)

	// Thermals
	if len(snap.Thermals) > 0 {
		fmt.Printf("├%s┤\n", line)
		for _, z := range snap.Thermals {
			bar := usageBar(z.TempC/100*100, 12)
			fmt.Printf("│  %-20s  %5.1f°C  %s  │\n", z.Name, z.TempC, bar)
		}
	}

	// Fans
	if len(snap.Fans) > 0 {
		fmt.Printf("├%s┤\n", line)
		for _, f := range snap.Fans {
			fmt.Printf("│  %-24s  %5d RPM  │\n", f.Name, f.RPM)
		}
	}

	fmt.Printf("└%s┘\n", line)
}

func usageBar(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled > width { filled = width }
	if filled < 0    { filled = 0 }
	bar   := strings.Repeat("█", filled)
	empty := strings.Repeat("░", width-filled)
	return bar + empty
}

func cmdCPU(args []string) {
	if len(args) < 2 || args[0] != "governor" {
		fmt.Fprintln(os.Stderr, "Usage: siliconctl cpu governor <name>")
		os.Exit(1)
	}
	governor := args[1]
	// Apply to all cores
	for i := 0; i < 128; i++ {
		err := hardware.SetCPUGovernor(i, governor)
		if err != nil && os.IsNotExist(err) { break }
		if err != nil {
			fmt.Fprintf(os.Stderr, "core%d: %v\n", i, err)
		} else {
			fmt.Printf("core%d → %s\n", i, governor)
		}
	}
}

func cmdFan(args []string) {
	if len(args) < 3 || args[0] != "set" {
		fmt.Fprintln(os.Stderr, "Usage: siliconctl fan set <name> <pct>")
		os.Exit(1)
	}
	fmt.Printf("[fan] %s → %s%% (requires hwmon write access)\n", args[1], args[2])
	// Real impl: locate hwmon path by name, then call SetFanPWM
}
