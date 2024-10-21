package coresmd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/coredhcp/coredhcp/handler"
	"github.com/coredhcp/coredhcp/logger"
	"github.com/coredhcp/coredhcp/plugins"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

type IfaceInfo struct {
	CompID  string
	CompNID int64
	Type    string
	MAC     string
	IPList  []net.IP
}

var log = logger.GetLogger("plugins/coresmd")

var Plugin = plugins.Plugin{
	Name:   "coresmd",
	Setup6: setup6,
	Setup4: setup4,
}

var (
	cache             *Cache
	baseURL           *url.URL
	bootScriptBaseURL *url.URL
)

func setup6(args ...string) (handler.Handler6, error) {
	return nil, errors.New("coresmd does not currently support DHCPv6")
}

func setup4(args ...string) (handler.Handler4, error) {
	// Ensure all required args were passed
	if len(args) != 4 {
		return nil, errors.New("expected 4 arguments: base URL, boot script base URL, CA certificate path, cache duration")
	}

	// Create new SmdClient using first argument (base URL)
	log.Debug("generating new SmdClient")
	var err error
	baseURL, err = url.Parse(args[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}
	smdClient := NewSmdClient(baseURL)

	// Parse from the second argument the insecure URL used by iPXE clients
	// to fetch their boot script via HTTP without a certificate
	log.Debug("parsing boot script base URL")
	bootScriptBaseURL, err = url.Parse(args[1])
	if err != nil {
		return nil, fmt.Errorf("failed to parse boot script base URL: %w", err)
	}

	// If nonempty, test that CA cert path exists (third argument)
	caCertPath := args[2]
	if caCertPath != "" {
		if err := smdClient.UseCACert(caCertPath); err != nil {
			return nil, fmt.Errorf("failed to set CA certificate: %w", err)
		}
		log.Infof("set CA certificate for SMD to the contents of %s", caCertPath)
	} else {
		log.Infof("CA certificate path was empty, not setting")
	}

	// Create new Cache using fourth argument (cache validity duration) and new SmdClient
	// pointer
	log.Debug("generating new Cache")
	cache, err = NewCache(args[3], smdClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create new cache: %w", err)
	}

	cache.RefreshLoop()

	log.Infof("coresmd plugin initialized with base URL %s and validity duration %s", smdClient.BaseURL, cache.Duration.String())

	return Handler4, nil
}

func Handler4(req, resp *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, bool) {
	log.Debugf("HANDLER CALLED ON MESSAGE TYPE: req(%s), resp(%s)", req.MessageType(), resp.MessageType())
	log.Debugf("REQUEST: %s", req.Summary())

	// Make sure cache doesn't get updated while reading
	(*cache).Mutex.RLock()
	defer cache.Mutex.RUnlock()

	// STEP 1: Assign IP address
	hwAddr := req.ClientHWAddr.String()
	ifaceInfo, err := lookupMAC(hwAddr)
	if err != nil {
		log.Errorf("IP lookup failed: %v", err)
		return resp, true
	}
	assignedIP := ifaceInfo.IPList[0].To4()
	log.Infof("assigning %s to %s (%s)", assignedIP, ifaceInfo.MAC, ifaceInfo.Type)
	resp.YourIPAddr = assignedIP

	// Set client hostname
	if ifaceInfo.Type == "Node" {
		resp.Options.Update(dhcpv4.OptHostName(fmt.Sprintf("nid%03d", ifaceInfo.CompNID)))
	}

	// STEP 2: Send boot config
	var terminate bool
	if cinfo := req.Options.Get(dhcpv4.OptionUserClassInformation); string(cinfo) != "iPXE" {
		// BOOT STAGE 1: Send iPXE bootloader over TFTP
		resp, terminate = serveIPXEBootloader(req, resp)
	} else {
		// BOOT STAGE 2: Send URL to BSS boot script
		bssURL := bootScriptBaseURL.JoinPath("/boot/v1/bootscript")
		bssURL.RawQuery = fmt.Sprintf("mac=%s", hwAddr)
		resp.Options.Update(dhcpv4.OptBootFileName(bssURL.String()))
		terminate = false
	}

	log.Debugf("resp (after): %v", resp)
	log.Debugf("RESPONSE: %s", resp.Summary())

	return resp, terminate
}

func serveIPXEBootloader(req, resp *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, bool) {
	if req.Options.Has(dhcpv4.OptionClientSystemArchitectureType) {
		var carch iana.Arch
		carchBytes := req.Options.Get(dhcpv4.OptionClientSystemArchitectureType)
		log.Debugf("client architecture of %s is %v (%q)", req.ClientHWAddr, carchBytes, string(carchBytes))
		carch = iana.Arch(binary.BigEndian.Uint16(carchBytes))
		switch carch {
		case iana.EFI_IA32:
			// iPXE legacy 32-bit x86 bootloader
			resp.Options.Update(dhcpv4.OptBootFileName("undionly.kpxe"))
			return resp, false
		case iana.EFI_X86_64:
			// iPXE 64-bit x86 bootloader
			resp.Options.Update(dhcpv4.OptBootFileName("ipxe.efi"))
			return resp, false
		default:
			log.Errorf("no iPXE bootloader available for unknown architecture: %d (%s)", carch, carch.String())
			return resp, true
		}
	} else {
		log.Errorf("client did not present an architecture, unable to provide correct iPXE bootloader")
		return resp, true
	}
}

func lookupMAC(mac string) (IfaceInfo, error) {
	var ii IfaceInfo

	// Match MAC address with EthernetInterface
	ei, ok := cache.EthernetInterfaces[mac]
	if !ok {
		return ii, fmt.Errorf("no EthernetInterfaces were found in cache for hardware address %s", mac)
	}
	ii.MAC = mac

	// If found, make sure Component exists with ID matching to EthernetInterface ID
	ii.CompID = ei.ComponentID
	log.Debugf("EthernetInterface found in cache for hardware address %s with ID %s", ii.MAC, ii.CompID)
	comp, ok := cache.Components[ii.CompID]
	if !ok {
		return ii, fmt.Errorf("no Component %s found in cache for EthernetInterface hardware address %s", ii.CompID, ii.MAC)
	}
	ii.Type = comp.Type
	log.Debugf("matching Component of type %s with ID %s found in cache for hardware address %s", ii.Type, ii.CompID, ii.MAC)
	if ii.Type == "Node" {
		ii.CompNID = comp.NID
	}
	if len(ei.IPAddresses) == 0 {
		return ii, fmt.Errorf("EthernetInterface for Component %s (type %s) contains no IP addresses for hardware address %s", ii.CompID, ii.Type, ii.MAC)
	}
	log.Debugf("IP addresses available for hardware address %s (Component %s of type %s): %v", ii.MAC, ii.CompID, ii.Type, ei.IPAddresses)
	var ipList []net.IP
	for _, ipStr := range ei.IPAddresses {
		ip := net.ParseIP(ipStr.IPAddress)
		ipList = append(ipList, ip)
	}
	ii.IPList = ipList

	return ii, nil
}
