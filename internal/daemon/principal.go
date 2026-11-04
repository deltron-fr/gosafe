package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/vishvananda/netlink"
)

type Principal struct {
	FcMgr  *FirecrackerManager
	Logger *slog.Logger
}

type FirecrackerManager struct {
	FcBaseDir   string
	FcFileCount int
}

type NetworkManager struct {
	TAPcount int
}

type Instance struct {
	ID     string
	CPU    float64
	Memory float64
}

func (f *FirecrackerManager) StartFirecracker() error {
	socketPath := fmt.Sprintf("firecracker_%d.socket", f.FcFileCount)
	apiSocket := filepath.Join(f.FcBaseDir, socketPath)

	logPath := fmt.Sprintf("firecracker_%d.log", f.FcFileCount)
	firecrackerLogsPath := filepath.Join(f.FcBaseDir, logPath)

	logFile, err := os.OpenFile(firecrackerLogsPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	if err := os.Remove(apiSocket); !errors.Is(err, os.ErrNotExist) {
		return err
	}

	_, err = os.Create(apiSocket)
	if err != nil {
		return err
	}

	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "firecracker", "--api-sock", apiSocket, "enable-pci")
	cmd.Dir = f.FcBaseDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// TODO: this is totally incomplete - a channel should be listening for a signal
	// to terminate the firecracker process, use select-case
	go func() {
		err := cmd.Run()
		if err != nil {
			fmt.Println("firecracker failed: ", err)
		}
	}()

	return nil
}

func (n *NetworkManager) SetupNetwork() error {
	tapdevName := fmt.Sprintf("tap%d", n.TAPcount)
	if err := SetupTap(tapdevName, "172.16.0.1/30"); err != nil {
		return err
	}

	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward")
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "iptables", "-P", "FORWARD", "ACCEPT")
	if err := cmd.Run(); err != nil {
		return err
	}

	_, err := GetDefaultInterface() // TODO: should return host interface
	if err != nil {
		return err
	}
	// TODO: not finished

	return nil
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

func (p *Principal) StartInstance(inst Instance) error {
	return nil
}
