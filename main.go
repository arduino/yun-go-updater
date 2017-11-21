package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	expect "github.com/google/goexpect"
	"github.com/pin/tftp"
	"github.com/pkg/errors"
	serial "go.bug.st/serial.v1"
	"go.bug.st/serial.v1/enumerator"
)

// readHandler is called when client starts file download from server
func readHandler(filename string, rf io.ReaderFrom) error {
	execDir, _ := os.Executable()
	file, err := os.Open(filepath.Join(execDir, "tftp", filename))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	n, err := rf.ReadFrom(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	fmt.Printf("%d bytes sent\n", n)
	return nil
}

func externalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("are you connected to the network?")
}

func serveTFTP() {
	// only read capabilities
	s := tftp.NewServer(readHandler, nil)
	s.SetTimeout(5 * time.Second) // optional
	go s.ListenAndServe(":69")    // blocks until s.Shutdown() is called
}

func main() {

	bootloaderFirmwareName := "u-boot_linino_lede.bin"
	sysupgradeFirmwareName := "lede-ar71xx-generic-arduino-yun-squashfs-sysupgrade.bin"

	flashBootloader := flag.Bool("bl", false, "Flash bootloader too (danger zone)")
	targetBoard := flag.String("board", "Yun", "Update to target board")
	flag.Parse()
	// serve tftp files
	serveTFTP()
	// get self ip addresses
	serverAddr, err := externalIP()
	if err != nil {
		fmt.Println("Could not get your IP address, check your network connection")
		os.Exit(1)
	}
	// remove last octect to get an available IP adress for the board
	ip := net.ParseIP(serverAddr)
	ip = ip.To4()
	ip[3] = 10
	for ip[3] < 255 {
		_, err := net.DialTimeout("tcp", ip.String(), 2*time.Second)
		if err != nil {
			break
		}
		ip[3]++
	}
	ipAddr := ip.String()
	fmt.Println("Using " + serverAddr + " as server address and " + ipAddr + " as board address")

	// get serial ports attached
	var serialPort enumerator.PortDetails
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		log.Fatal(err)
	}
	if len(ports) == 0 {
		fmt.Println("No serial ports found!")
		return
	}
	for _, port := range ports {
		if port.IsUSB {
			fmt.Printf("Found port: %s\n", port.Name)
			fmt.Printf("USB ID     %s:%s\n", port.VID, port.PID)
			fmt.Printf("USB serial %s\n", port.SerialNumber)
			if canUse(port) {
				fmt.Println("Using it")
				serialPort = *port
				break
			}
		}
	}

	// upload the YunSerialTerminal to the board
	port, err := upload(serialPort.Name)
	if err != nil {
		log.Fatal(err)
	}

	// start the expecter
	exp, _, err := serialSpawn(port, time.Duration(10)*time.Second, expect.Verbose(true))
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		if err := exp.Close(); err != nil {
			fmt.Println("Problems when closing port")
		}
	}()

	if *flashBootloader {
		// set server and board ip
		exp.ExpectBatch([]expect.Batcher{
			&expect.BExp{R: "autoboot in 4 seconds:"},
			&expect.BSnd{S: "ard\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "setenv serverip " + serverAddr + "\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "setenv ipaddr " + ipAddr + "\n"},
			&expect.BExp{R: "linino>"},
		}, time.Duration(10)*time.Second)

		// flash new bootloader
		exp.ExpectBatch([]expect.Batcher{
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "tftp 0x80060000 " + bootloaderFirmwareName + "\n"},
			&expect.BExp{R: "Bytes transferred = 182492 (2c8dc hex)"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "erase 0x9f000000 +0x40000\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "cp.b $fileaddr 0x9f000000 $filesize\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "erase 0x9f040000 +0x10000\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "reset"},
		}, time.Duration(30)*time.Second)

		// set new name
		exp.ExpectBatch([]expect.Batcher{
			&expect.BExp{R: "autoboot in 4 seconds:"},
			&expect.BSnd{S: "ard\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "setenv board " + *targetBoard + "\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "saveenv\n"},
			&expect.BExp{R: "linino>"},
			&expect.BSnd{S: "reset"},
		}, time.Duration(10)*time.Second)
	}

	// set server and board ip
	exp.ExpectBatch([]expect.Batcher{
		&expect.BExp{R: "autoboot in 4 seconds:"},
		&expect.BSnd{S: "ard\n"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "setenv serverip " + serverAddr + "\n"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "setenv ipaddr " + ipAddr + "\n"},
		&expect.BExp{R: "linino>"},
	}, time.Duration(10)*time.Second)

	// flash sysupgrade
	exp.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "tftp 0x80060000 " + sysupgradeFirmwareName + "\n"},
		&expect.BExp{R: "Bytes transferred = 3538948 (360004 hex)"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "erase 0x9f050000 +0x400004\n"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "cp.b $fileaddr 0x9f050000 $filesize\n"},
		&expect.BExp{R: "linino>"},
		&expect.BSnd{S: "reset"},
	}, time.Duration(30)*time.Second)
}

func serialSpawn(port string, timeout time.Duration, opts ...expect.Option) (expect.Expecter, <-chan error, error) {
	// open the port with safe parameters
	mode := &serial.Mode{
		BaudRate: 9600,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	}
	serPort, err := serial.Open(port, mode)
	if err != nil {
		return nil, nil, err
	}

	resCh := make(chan error)

	return expect.SpawnGeneric(&expect.GenOptions{
		In:  serPort,
		Out: serPort,
		Wait: func() error {
			return <-resCh
		},
		Close: func() error {
			close(resCh)
			return serPort.Close()
		},
		Check: func() bool { return true },
	}, timeout, opts...)
}

func upload(port string) (string, error) {
	port, err := reset(port, true)
	if err != nil {
		return "", err
	}
	execDir, _ := os.Executable()
	binDir := filepath.Join(execDir, "avr")
	FWName := "YunSerialTerminal.ino.hex"
	args := []string{"-C" + binDir + "/etc/avrdude.conf", "-v", "-patmega32u4", "-cavr109", "-P" + port, "-b57600", "-D", "-Uflash:w:" + FWName + ":i"}
	err = program(filepath.Join(binDir, "bin", "avrdude"), args)
	if err != nil {
		return "", err
	}
	ports, err := serial.GetPortsList()
	port = waitReset(ports, port, 1)
	return port, nil
}

// program spawns the given binary with the given args, logging the sdtout and stderr
// through the Logger
func program(binary string, args []string) error {
	// remove quotes form binary command and args
	binary = strings.Replace(binary, "\"", "", -1)

	for i := range args {
		args[i] = strings.Replace(args[i], "\"", "", -1)
	}

	// find extension
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}

	cmd := exec.Command(binary, args...)

	//utilities.TellCommandNotToSpawnShell(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrapf(err, "Retrieve output")
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errors.Wrapf(err, "Retrieve output")
	}

	fmt.Println("Flashing with command:" + binary + extension + " " + strings.Join(args, " "))

	err = cmd.Start()

	stdoutCopy := bufio.NewScanner(stdout)
	stderrCopy := bufio.NewScanner(stderr)

	stdoutCopy.Split(bufio.ScanLines)
	stderrCopy.Split(bufio.ScanLines)

	go func() {
		for stdoutCopy.Scan() {
			fmt.Println(stdoutCopy.Text())
		}
	}()

	go func() {
		for stderrCopy.Scan() {
			fmt.Println(stderrCopy.Text())
		}
	}()

	err = cmd.Wait()
	if err != nil {
		return errors.Wrapf(err, "Executing command")
	}
	return nil
}

// reset opens the port at 1200bps. It returns the new port name (which could change
// sometimes) and an error (usually because the port listing failed)
func reset(port string, wait bool) (string, error) {
	fmt.Println("Restarting in bootloader mode")

	// Get port list before reset
	ports, err := serial.GetPortsList()
	fmt.Println("Get port list before reset")
	if err != nil {
		return "", errors.Wrapf(err, "Get port list before reset")
	}

	// Touch port at 1200bps
	err = touchSerialPortAt1200bps(port)
	if err != nil {
		return "", errors.Wrapf(err, "1200bps Touch")
	}

	// Wait for port to disappear and reappear
	if wait {
		port = waitReset(ports, port, 10)
	}

	return port, nil
}

func touchSerialPortAt1200bps(port string) error {
	// Open port
	p, err := serial.Open(port, &serial.Mode{BaudRate: 1200})
	if err != nil {
		errors.Wrapf(err, "Open port %s", port)
	}
	defer p.Close()

	// Set DTR
	err = p.SetDTR(false)
	if err != nil {
		errors.Wrapf(err, "Can't set DTR")
	}

	// Wait a bit to allow restart of the board
	time.Sleep(200 * time.Millisecond)

	return nil
}

// waitReset is meant to be called just after a reset. It watches the ports connected
// to the machine until a port disappears and reappears. The port name could be different
// so it returns the name of the new port.
func waitReset(beforeReset []string, originalPort string, timeout_len int) string {
	var port string
	timeout := false

	go func() {
		time.Sleep(time.Duration(timeout_len) * time.Second)
		timeout = true
	}()

	// Wait for the port to disappear
	fmt.Println("Wait for the port to disappear")
	for {
		ports, err := serial.GetPortsList()
		port = differ(ports, beforeReset)
		fmt.Println(beforeReset, " -> ", ports)

		if port != "" {
			break
		}
		if timeout {
			fmt.Println(ports, err, port)
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	// Wait for the port to reappear
	fmt.Println("Wait for the port to reappear")
	afterReset, _ := serial.GetPortsList()
	for {
		ports, _ := serial.GetPortsList()
		port = differ(ports, afterReset)
		fmt.Println(afterReset, " -> ", ports)
		if port != "" {
			fmt.Println("Found upload port: ", port)
			time.Sleep(time.Millisecond * 500)
			break
		}
		if timeout {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	// try to upload on the existing port if the touch was ineffective
	if port == "" {
		port = originalPort
	}

	return port
}

// differ returns the first item that differ between the two input slices
func differ(slice1 []string, slice2 []string) string {
	m := map[string]int{}

	for _, s1Val := range slice1 {
		m[s1Val] = 1
	}
	for _, s2Val := range slice2 {
		m[s2Val] = m[s2Val] + 1
	}

	for mKey, mVal := range m {
		if mVal == 1 {
			return mKey
		}
	}

	return ""
}

func canUse(port *enumerator.PortDetails) bool {
	if port.VID == "2341" && port.PID == "8041" {
		return true
	}
	return false
}