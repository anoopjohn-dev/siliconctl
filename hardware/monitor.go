// siliconctl/hardware/monitor.go
// Reads CPU, GPU, thermal, fan data from sysfs / procfs / hwmon

package hardware

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Data types ────────────────────────────────────────────────────────────────

type CPUCore struct {
	ID       int     `json:"id"`
	UsagePct float64 `json:"usage_pct"`
	FreqMHz  int     `json:"freq_mhz"`
	TempC    float64 `json:"temp_c,omitempty"`
}

type MemInfo struct {
	TotalMB int `json:"total_mb"`
	UsedMB  int `json:"used_mb"`
	FreeMB  int `json:"free_mb"`
	PctUsed int `json:"pct_used"`
}

type ThermalZone struct {
	Name   string  `json:"name"`
	TempC  float64 `json:"temp_c"`
	Policy string  `json:"policy,omitempty"`
}

type FanInfo struct {
	Name    string `json:"name"`
	RPM     int    `json:"rpm"`
	PWMPct  int    `json:"pwm_pct,omitempty"`
}

type Snapshot struct {
	Timestamp int64         `json:"ts"`
	Cores     []CPUCore     `json:"cores"`
	Memory    MemInfo       `json:"memory"`
	Thermals  []ThermalZone `json:"thermals"`
	Fans      []FanInfo     `json:"fans"`
}

// ── Monitor ───────────────────────────────────────────────────────────────────

type Monitor struct {
	prevStat []cpuStat
	mu       sync.Mutex
}

func New() *Monitor { return &Monitor{} }

func (m *Monitor) Snapshot() (*Snapshot, error) {
	s := &Snapshot{Timestamp: time.Now().UnixMilli()}

	var wg sync.WaitGroup
	var errCores, errMem, errTherm, errFans error

	wg.Add(4)
	go func() { defer wg.Done(); s.Cores, errCores = m.readCPU() }()
	go func() { defer wg.Done(); s.Memory, errMem = readMemInfo() }()
	go func() { defer wg.Done(); s.Thermals, errTherm = readThermals() }()
	go func() { defer wg.Done(); s.Fans, errFans = readFans() }()
	wg.Wait()

	for _, e := range []error{errCores, errMem, errTherm, errFans} {
		if e != nil {
			return s, e
		}
	}
	return s, nil
}

// ── CPU ───────────────────────────────────────────────────────────────────────

type cpuStat struct {
	user, nice, system, idle, iowait, irq, softirq uint64
}

func (s cpuStat) total() uint64 {
	return s.user + s.nice + s.system + s.idle + s.iowait + s.irq + s.softirq
}
func (s cpuStat) active() uint64 {
	return s.total() - s.idle - s.iowait
}

func parseCPUStat(line string) (cpuStat, int, error) {
	var id int
	var s cpuStat
	// "cpu0  12345 0 6789 ..."
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return s, -1, fmt.Errorf("short stat line")
	}
	n, _ := fmt.Sscanf(fields[0], "cpu%d", &id)
	if n == 0 {
		return s, -1, fmt.Errorf("not a cpu line")
	}
	vals := []*uint64{&s.user, &s.nice, &s.system, &s.idle,
		&s.iowait, &s.irq, &s.softirq}
	for i, v := range vals {
		*v, _ = strconv.ParseUint(fields[i+1], 10, 64)
	}
	return s, id, nil
}

func (m *Monitor) readCPU() ([]CPUCore, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var stats []cpuStat
	var ids   []int

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") || strings.HasPrefix(line, "cpu ") {
			continue
		}
		st, id, err := parseCPUStat(line)
		if err == nil {
			stats = append(stats, st)
			ids   = append(ids, id)
		}
	}

	m.mu.Lock()
	prev := m.prevStat
	m.prevStat = stats
	m.mu.Unlock()

	cores := make([]CPUCore, len(stats))
	for i, cur := range stats {
		cores[i].ID = ids[i]
		if i < len(prev) {
			dt := float64(cur.total() - prev[i].total())
			da := float64(cur.active() - prev[i].active())
			if dt > 0 {
				cores[i].UsagePct = da / dt * 100
			}
		}
		cores[i].FreqMHz = readCPUFreq(ids[i])
	}
	return cores, nil
}

func readCPUFreq(core int) int {
	path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", core)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	khz, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return khz / 1000
}

// ── Memory ────────────────────────────────────────────────────────────────────

func readMemInfo() (MemInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()

	kv := make(map[string]int)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 2 {
			v, _ := strconv.Atoi(parts[1])
			kv[strings.TrimSuffix(parts[0], ":")] = v
		}
	}

	total := kv["MemTotal"] / 1024
	avail := kv["MemAvailable"] / 1024
	used  := total - avail
	pct   := 0
	if total > 0 {
		pct = used * 100 / total
	}
	return MemInfo{TotalMB: total, UsedMB: used, FreeMB: avail, PctUsed: pct}, nil
}

// ── Thermals ─────────────────────────────────────────────────────────────────

func readThermals() ([]ThermalZone, error) {
	dirs, err := filepath.Glob("/sys/class/thermal/thermal_zone*")
	if err != nil || len(dirs) == 0 {
		return nil, err
	}

	var zones []ThermalZone
	for _, d := range dirs {
		tempData, err := os.ReadFile(filepath.Join(d, "temp"))
		if err != nil {
			continue
		}
		milli, _ := strconv.Atoi(strings.TrimSpace(string(tempData)))

		typeData, _ := os.ReadFile(filepath.Join(d, "type"))
		name := strings.TrimSpace(string(typeData))

		zones = append(zones, ThermalZone{
			Name:  name,
			TempC: float64(milli) / 1000.0,
		})
	}
	return zones, nil
}

// ── Fans ─────────────────────────────────────────────────────────────────────

func readFans() ([]FanInfo, error) {
	dirs, err := filepath.Glob("/sys/class/hwmon/hwmon*")
	if err != nil {
		return nil, err
	}

	var fans []FanInfo
	for _, d := range dirs {
		// Each hwmon device can have fan1_input, fan2_input, etc.
		inputs, _ := filepath.Glob(filepath.Join(d, "fan*_input"))
		for _, inp := range inputs {
			data, err := os.ReadFile(inp)
			if err != nil {
				continue
			}
			rpm, _ := strconv.Atoi(strings.TrimSpace(string(data)))

			// Derive name from hwmon name file + fan index
			nameData, _ := os.ReadFile(filepath.Join(d, "name"))
			hwName := strings.TrimSpace(string(nameData))
			idx    := strings.TrimSuffix(filepath.Base(inp), "_input")

			fans = append(fans, FanInfo{
				Name: fmt.Sprintf("%s_%s", hwName, idx),
				RPM:  rpm,
			})
		}
	}
	return fans, nil
}

// SetFanPWM writes a PWM value (0-255) to a hwmon fan
func SetFanPWM(hwmonPath string, fanIdx int, pwm int) error {
	if pwm < 0 || pwm > 255 {
		return fmt.Errorf("PWM value %d out of range [0, 255]", pwm)
	}
	path := fmt.Sprintf("%s/pwm%d", hwmonPath, fanIdx)
	return os.WriteFile(path, []byte(strconv.Itoa(pwm)), 0644)
}

// SetCPUGovernor writes a scaling governor for a CPU core
func SetCPUGovernor(core int, governor string) error {
	valid := map[string]bool{
		"performance": true, "powersave": true,
		"ondemand": true, "conservative": true, "schedutil": true,
	}
	if !valid[governor] {
		return fmt.Errorf("unknown governor %q", governor)
	}
	path := fmt.Sprintf(
		"/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", core)
	return os.WriteFile(path, []byte(governor), 0644)
}
