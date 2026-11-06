package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/google/nftables"

	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
)

type Principal struct {
	FcMgr     *FirecrackerManager
	TapCount  int
	Instances map[int]Instance
	Logger    *slog.Logger
}

type FirecrackerManager struct {
	Processes   map[int]*FirecrackerProcess
	FcBaseDir   string
	FcFileCount int
}

type FirecrackerProcess struct {
	PID     int
	Socket  string
	LogPath string
}

type NetworkConfig struct {
	TAPDeviceName string
	MACAddr       string
	HostIP        string
}

type Instance struct {
	ID            string
	CPU           float64
	Memory        float64
	NetworkConfig NetworkConfig
}

func (f *FirecrackerManager) StartFirecracker() (int, error) {
	wg := sync.WaitGroup{}

	socketPath := fmt.Sprintf("firecracker_%d.socket", f.FcFileCount)
	apiSocket := filepath.Join(f.FcBaseDir, socketPath)

	logPath := fmt.Sprintf("firecracker_%d.log", f.FcFileCount)
	firecrackerLogsPath := filepath.Join(f.FcBaseDir, logPath)

	logFile, err := os.OpenFile(firecrackerLogsPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	if err := os.Remove(apiSocket); !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	_, err = os.Create(apiSocket)
	if err != nil {
		return 0, err
	}

	fc := &FirecrackerProcess{Socket: apiSocket, LogPath: logPath}

	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "firecracker", "--api-sock", apiSocket, "enable-pci")
	cmd.Dir = f.FcBaseDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// TODO: this is totally incomplete - a channel should be listening for a signal
	// to terminate the firecracker process, use select-case
	wg.Add(1)
	go func() {
		err := cmd.Start()
		if err != nil {
			fmt.Println("firecracker failed: ", err)
		}

		fc.PID = cmd.Process.Pid
		wg.Done()

		if err = cmd.Run(); err != nil {
			fmt.Println("firecracker failed: ", err)
		}
	}()
	wg.Wait()

	f.FcFileCount++
	id := f.FcFileCount
	f.Processes[id] = fc

	return id, nil
}

func (p *Principal) ConfigureFirecracker(processID int, inst *Instance) error {
	fc := p.FcMgr.Processes[processID]
	c := newUnixClient(fc.Socket)

	type logReq struct {
		LogPath       string `json:"log_path"`
		Level         string `json:"level"`
		ShowLevel     bool   `json:"show_level"`
		ShowLogOrigin bool   `json:"show_log_origin"`
	}

	js, err := json.Marshal(logReq{
		LogPath:       fc.LogPath,
		Level:         "Debug",
		ShowLevel:     true,
		ShowLogOrigin: true,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err = makeRequest(ctx, c, "PUT", "http://localhost/logger", js); err != nil {
		return err
	}

	kernel := "./vmlinux-6.18.56"
	kernelBootArgs := "console=ttyS0 reboot=k panic=1"

	if runtime.GOARCH == "arm64" {
		kernelBootArgs = fmt.Sprintf("keep_bootcon %s", kernelBootArgs)
	}

	type bootSourceReq struct {
		KernelImage string `json:"kernel_image_path"`
		BootArgs    string `json:"boot_args"`
	}

	js, err = json.Marshal(bootSourceReq{
		KernelImage: kernel,
		BootArgs:    kernelBootArgs,
	})
	if err != nil {
		return err
	}

	if err = makeRequest(ctx, c, "PUT", "http://localhost/boot-source", js); err != nil {
		return err
	}

	rootfs := "ubuntu-24.04.ext4"

	type rootfsReq struct {
		DriveID    string `json:"drive_id"`
		PathOnHost string `json:"path_on_host"`
		RootDevice bool   `json:"is_root_device"`
		ReadOnly   bool   `json:"is_read_only"`
	}

	js, err = json.Marshal(rootfsReq{
		DriveID:    "rootfs",
		PathOnHost: rootfs,
		RootDevice: true,
		ReadOnly:   false,
	})
	if err != nil {
		return err
	}

	if err = makeRequest(ctx, c, "PUT", "http://localhost/drives/rootfs", js); err != nil {
		return err
	}

	interfaceID := fmt.Sprintf("net%d", p.TapCount)
	mac, err := GenerateMAC()
	if err != nil {
		return err
	}

	type netInterfaceReq struct {
		InterfaceID       string `json:"iface_id"`
		GuestMac          string `json:"guest_mac"`
		HostTapDeviceName string `json:"host_dev_name"`
	}

	js, err = json.Marshal(netInterfaceReq{
		InterfaceID:       interfaceID,
		GuestMac:          mac.String(),
		HostTapDeviceName: inst.NetworkConfig.TAPDeviceName,
	})
	if err != nil {
		return err
	}

	if err = makeRequest(ctx, c, "PUT", fmt.Sprintf("http://localhost/network-interfaces/%s", interfaceID), js); err != nil {
		return err
	}

	return nil
}

func makeRequest(ctx context.Context, c *http.Client, method, url string, jsonData []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return fmt.Errorf("something unexpected happened with the request")
	}

	return nil
}

func (p *Principal) SetupNetwork(inst *Instance) error {
	p.TapCount++
	tapdevName := fmt.Sprintf("tap%d", p.TapCount)
	if err := SetupTap(tapdevName, "172.16.0.1/30"); err != nil {
		return err
	}

	inst.NetworkConfig.TAPDeviceName = tapdevName

	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward")
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "iptables", "-P", "FORWARD", "ACCEPT")
	if err := cmd.Run(); err != nil {
		return err
	}

	hostInterface, err := GetDefaultInterface()
	if err != nil {
		return err
	}

	if err = SetupRoutingRules(hostInterface); err != nil {
		return err
	}

	return nil
}

func SetupRoutingRules(hostInterface string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "nat",
	})

	priority := nftables.ChainPriorityNATSource
	postrouteChain := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: priority,
	})

	expressions := []expr.Any{
		&expr.Meta{
			Key:      expr.MetaKeyOIFNAME,
			Register: 1,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     encodeIfName(hostInterface),
		},
		&expr.Masq{},
	}

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: postrouteChain,
		Exprs: expressions,
	})

	if err := conn.Flush(); err != nil {
		return err
	}

	return nil
}

func encodeIfName(name string) []byte {
	return append([]byte(name), 0x00)
}

func GetDefaultInterface() (string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", fmt.Errorf("failed to list routes: %w", err)
	}

	for _, route := range routes {
		// A default route has a nil destination (0.0.0.0/0)
		if route.Dst == nil || route.Dst.IP.IsUnspecified() {
			link, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("failed to get link by index %d: %w", route.LinkIndex, err)
			}
			return link.Attrs().Name, nil
		}
	}

	return "", fmt.Errorf("no default route found")
}

func SetupTap(name, address string) error {
	link, err := netlink.LinkByName(name)
	if err == nil {
		if err = netlink.LinkDel(link); err != nil {
			return err
		}
	}

	linkAttr := netlink.NewLinkAttrs()
	linkAttr.Name = name

	tapdev := &netlink.Tuntap{
		LinkAttrs: linkAttr,
		Mode:      netlink.TUNTAP_MODE_TAP,
	}

	if err = netlink.LinkAdd(tapdev); err != nil {
		return err
	}

	addr, err := netlink.ParseAddr(address)
	if err != nil {
		return err
	}

	if err := netlink.AddrAdd(tapdev, addr); err != nil {
		return err
	}

	return netlink.LinkSetUp(tapdev)
}

func GenerateMAC() (net.HardwareAddr, error) {
	mac := make([]byte, 6)

	if _, err := rand.Read(mac); err != nil {
		return nil, err
	}

	// Set locally administered bit and clear multicast bit.
	mac[0] = (mac[0] | 2) & 0xfe

	return net.HardwareAddr(mac), nil
}

func (p *Principal) StartInstance(inst Instance) error {
	return nil
}

func newUnixClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}
