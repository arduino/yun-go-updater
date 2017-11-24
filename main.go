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
	"strconv"
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
	execDir = filepath.Dir(execDir)
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

func externalIP(notThis string) (string, error) {
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
			if ip == nil || ip.String() == notThis {
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
	go func() {
		time.Sleep(1 * time.Second)
		s.Shutdown()
	}()
	err := s.ListenAndServe(":69") // blocks until s.Shutdown() is called
	if err != nil {
		log.Fatal("Can't spawn tftp server, make sure you are running as administrator\n" + err.Error())
	}
	// respawn as goroutine
	go s.ListenAndServe(":69")
}

func getServerAndBoardIP(serverAddr, ipAddr *string) {
	// get self ip addresses
	var err error
	*serverAddr, err = externalIP(*serverAddr)
	if err != nil {
		fmt.Println("Could not get your IP address, check your network connection")
		os.Exit(1)
	}
	// remove last octect to get an available IP adress for the board
	ip := net.ParseIP(*serverAddr)
	ip = ip.To4()
	// strat trying from server IP + 1
	ip[3]++
	for ip[3] < 255 {
		_, err := net.DialTimeout("tcp", ip.String(), 2*time.Second)
		if err != nil {
			break
		}
		ip[3]++
	}
	*ipAddr = ip.String()
}

type firmwareFile struct {
	name string
	size int64
}

type context struct {
	flashBootloader    *bool
	serverAddr         string
	ipAddr             string
	bootloaderFirmware firmwareFile
	sysupgradeFirmware firmwareFile
	targetBoard        *string
}

func getFileSize(path string) int64 {
	file, _ := os.Open(path)
	fi, _ := file.Stat()
	return fi.Size()
}

func main() {

	bootloaderFirmwareName := "u-boot-arduino-lede.bin"
	sysupgradeFirmwareName := "ledeyun-17.11-r5403+1-3e7b776-ar71xx-generic-arduino-yun-squashfs-sysupgrade.bin"

	serverAddr := ""
	ipAddr := ""

	oldYun := flag.Bool("old", false, "Flash really old Yun")
	flashBootloader := flag.Bool("bl", false, "Flash bootloader too (danger zone)")
	targetBoard := flag.String("board", "Yun", "Update to target board")
	flag.Parse()
	// serve tftp files
	serveTFTP()
	getServerAndBoardIP(&serverAddr, &ipAddr)
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

	if serialPort.Name == "" {
		log.Fatal("No serial port suitable for updating " + *targetBoard)
	}

	// upload the YunSerialTerminal to the board
	port, err := upload(serialPort.Name)
	if err != nil {
		log.Fatal(err)
	}

	// start the expecter
	exp, _, err := serialSpawn(port, time.Duration(10)*time.Second, expect.CheckDuration(100*time.Millisecond), expect.Verbose(false), expect.VerboseWriter(os.Stdout))
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		if err := exp.Close(); err != nil {
			fmt.Println("Problems when closing port")
		}
	}()

	execDir, _ := os.Executable()
	execDir = filepath.Dir(execDir)
	tftpDir := filepath.Join(execDir, "tftp")

	bootloaderSize := getFileSize(filepath.Join(tftpDir, bootloaderFirmwareName))
	sysupgradeSize := getFileSize(filepath.Join(tftpDir, sysupgradeFirmwareName))

	bootloaderFirmware := firmwareFile{name: bootloaderFirmwareName, size: bootloaderSize}
	sysupgradeFirmware := firmwareFile{name: sysupgradeFirmwareName, size: sysupgradeSize}

	ctx := context{flashBootloader: flashBootloader, serverAddr: serverAddr, ipAddr: ipAddr, bootloaderFirmware: bootloaderFirmware, sysupgradeFirmware: sysupgradeFirmware, targetBoard: targetBoard}

	lastline, err := flash(exp, ctx, *oldYun)

	retry_count := 0

	for err != nil && retry_count < 3 /* && strings.Contains(lastline, "Loading: T ")*/ {
		//retry with different IP addresses
		fmt.Println(lastline)
		fmt.Println(err.Error())
		getServerAndBoardIP(&serverAddr, &ipAddr)
		ctx.serverAddr = serverAddr
		ctx.ipAddr = ipAddr
		retry_count++
		lastline, err = flash(exp, ctx, *oldYun)
	}

	if err == nil {
		fmt.Println("All done! Enjoy your updated " + *targetBoard)
	}
	//fmt.Println(lastline)
}

func flash(exp expect.Expecter, ctx context, superOldYun bool) (string, error) {
	res, err := exp.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BExp{R: "root@"},
		&expect.BSnd{S: "reboot -f\n"},
	}, time.Duration(5)*time.Second)

	if err != nil {
		fmt.Println("Reboot the board using YUN RST button")
	} else {
		fmt.Println("Rebooting the board")
	}

	err = nil

	if !superOldYun {
		// in bootloader mode:
		// understand which version of the BL we are in
		res, err = exp.ExpectBatch([]expect.Batcher{
			&expect.BExp{R: "(stop with '([a-z]+)'|Hit any key to stop autoboot|type '([a-z]+)' to enter u-boot console)"},
		}, time.Duration(20)*time.Second)
	}

	if err != nil {
		return "", err
	}

	stopCommand := res[0].Match[len(res[0].Match)-1]

	if stopCommand == "" {
		stopCommand = res[0].Match[len(res[0].Match)-2]
	}

	if res[0].Match[0] == "Hit any key to stop autoboot" {
		fmt.Println("Old YUN detected")
		stopCommand = ""
	}

	fmt.Println("Using stop command: " + stopCommand)

	// call stop and detect firmware version (if it needs to be updated)
	res, err = exp.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: stopCommand + "\n"},
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "([0-9a-zA-Z]+)>"},
	}, time.Duration(5)*time.Second)

	if err != nil {
		return "", err
	}

	fwShell := res[0].Match[len(res[0].Match)-1]
	fmt.Println("Got shell: " + fwShell)

	if fwShell != "arduino" {
		*ctx.flashBootloader = true
		fmt.Println("fwShell: " + fwShell)
	}

	time.Sleep(1 * time.Second)

	if *ctx.flashBootloader {

		fmt.Println("Flashing Bootloader")

		// set server and board ip
		res, err = exp.ExpectBatch([]expect.Batcher{
			&expect.BSnd{S: "setenv serverip " + ctx.serverAddr + "\n"},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: "serverip=" + ctx.serverAddr},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "setenv ipaddr " + ctx.ipAddr + "\n"},
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: "ipaddr=" + ctx.ipAddr},
			&expect.BExp{R: fwShell + ">"},
		}, time.Duration(10)*time.Second)

		if err != nil {
			return res[len(res)-1].Output, err
		}

		time.Sleep(2 * time.Second)

		// flash new bootloader
		res, err = exp.ExpectBatch([]expect.Batcher{
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "tftp 0x80060000 " + ctx.bootloaderFirmware.name + "\n"},
			&expect.BExp{R: "Bytes transferred = " + strconv.FormatInt(ctx.bootloaderFirmware.size, 10)},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "erase 0x9f000000 +0x40000\n"},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "cp.b $fileaddr 0x9f000000 $filesize\n"},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "erase 0x9f040000 +0x10000\n"},
			&expect.BExp{R: fwShell + ">"},
			&expect.BSnd{S: "reset\n"},
		}, time.Duration(30)*time.Second)

		if err != nil {
			return res[len(res)-1].Output, err
		}

		// New bootloader flashed, stop with 'ard' and shell is 'arduino>'

		time.Sleep(1 * time.Second)

		// set new name
		res, err = exp.ExpectBatch([]expect.Batcher{
			&expect.BExp{R: "autoboot in"},
			&expect.BSnd{S: "ard\n"},
			&expect.BExp{R: "arduino>"},
			&expect.BSnd{S: "printenv\n"},
			&expect.BExp{R: "arduino>"},
			&expect.BSnd{S: "setenv board " + *ctx.targetBoard + "\n"},
			&expect.BExp{R: "arduino>"},
			&expect.BSnd{S: "saveenv\n"},
			&expect.BExp{R: "arduino>"},
		}, time.Duration(10)*time.Second)
	}

	if err != nil {
		return res[len(res)-1].Output, err
	}

	fmt.Println("Setting up IP addresses")

	// set server and board ip
	res, err = exp.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: "setenv serverip " + ctx.serverAddr + "\n"},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "serverip=" + ctx.serverAddr},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "setenv ipaddr " + ctx.ipAddr + "\n"},
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "ipaddr=" + ctx.ipAddr},
		&expect.BExp{R: "arduino>"},
	}, time.Duration(20)*time.Second)

	if err != nil {
		return res[len(res)-1].Output, err
	}

	fmt.Println("Flashing sysupgrade image")

	// ping the serverIP; if ping is not working, try another network interface
	/*
		res, err = exp.ExpectBatch([]expect.Batcher{
			&expect.BSnd{S: "ping " + ctx.serverAddr + "\n"},
			&expect.BExp{R: "is alive"},
		}, time.Duration(6)*time.Second)

		if err != nil {
			return res[len(res)-1].Output, err
		}
	*/

	time.Sleep(2 * time.Second)

	// flash sysupgrade
	res, err = exp.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "board="},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "tftp 0x80060000 " + ctx.sysupgradeFirmware.name + "\n"},
		&expect.BExp{R: "Bytes transferred = " + strconv.FormatInt(ctx.sysupgradeFirmware.size, 10)},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "erase 0x9f050000 +0x" + strconv.FormatInt(ctx.sysupgradeFirmware.size, 16) + "\n"},
		&expect.BExp{R: "Erased [0-9]+ sectors"},
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "cp.b $fileaddr 0x9f050000 $filesize\n"},
		&expect.BExp{R: "done"},
		&expect.BSnd{S: "printenv\n"},
		&expect.BExp{R: "arduino>"},
		&expect.BSnd{S: "reset\n"},
		&expect.BExp{R: "Starting kernel"},
	}, time.Duration(60)*time.Second)

	if err != nil {
		return res[len(res)-1].Output, err
	}

	return res[len(res)-1].Output, nil
}

func serialSpawn(port string, timeout time.Duration, opts ...expect.Option) (expect.Expecter, <-chan error, error) {
	// open the port with safe parameters
	mode := &serial.Mode{
		BaudRate: 115200,
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

	time.Sleep(1 * time.Second)

	execDir, _ := os.Executable()
	execDir = filepath.Dir(execDir)
	binDir := filepath.Join(execDir, "avr")
	FWName := filepath.Join(binDir, "YunSerialTerminal.ino.hex")
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
			//fmt.Println(stdoutCopy.Text())
		}
	}()

	go func() {
		for stderrCopy.Scan() {
			//fmt.Println(stderrCopy.Text())
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
		//fmt.Println(beforeReset, " -> ", ports)

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
		//fmt.Println(afterReset, " -> ", ports)
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
	if port.VID == "2341" && (port.PID == "8041" || port.PID == "0041") {
		return true
	}
	if port.VID == "2a03" && (port.PID == "8041" || port.PID == "0041") {
		return true
	}
	return false
}
