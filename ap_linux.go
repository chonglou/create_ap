package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"syscall"
	"time"
)

const CHANNEL_AUTO = 0

func (ap *AccessPoint) start() error {
	var err error

	hwmodes := ap.wifiIf.hwmodes
	s := "Your adapter does not support transmission on 802.11"

	// If IEEE 802.11 protocol is not selected then we need to choose the most
	// recent one that adapter supports.
	if ap.ieee80211 == "auto" {
		switch {
		case ap.channel == CHANNEL_AUTO:
			switch {
			case hwmodes&IEEE80211_AC != 0:
				ap.ieee80211 = "ac"
			case hwmodes&IEEE80211_N != 0:
				ap.ieee80211 = "n"
			case hwmodes&IEEE80211_G != 0:
				ap.ieee80211 = "g"
			case hwmodes&IEEE80211_A != 0:
				ap.ieee80211 = "a"
			default:
				return fmt.Errorf(s + "a/g/n/ac")
			}
		case ap.channel >= 1 && ap.channel <= 14:
			switch {
			case hwmodes&IEEE80211_N != 0:
				ap.ieee80211 = "n"
			case hwmodes&IEEE80211_G != 0:
				ap.ieee80211 = "g"
			default:
				return fmt.Errorf(s + "g/n")
			}
		default:
			switch {
			case hwmodes&IEEE80211_AC != 0:
				ap.ieee80211 = "ac"
			case hwmodes&IEEE80211_N != 0:
				ap.ieee80211 = "n"
			case hwmodes&IEEE80211_A != 0:
				ap.ieee80211 = "a"
			default:
				return fmt.Errorf(s + "a/n/ac")
			}
		}
	}

	switch ap.ieee80211 {
	case "a":
		if hwmodes&IEEE80211_A == 0 {
			return fmt.Errorf(s + "a")
		}
	case "g":
		if hwmodes&IEEE80211_G == 0 {
			return fmt.Errorf(s + "g")
		}
	case "n":
		if hwmodes&IEEE80211_N == 0 {
			return fmt.Errorf(s + "n")
		}
	case "ac":
		if hwmodes&IEEE80211_AC == 0 {
			return fmt.Errorf(s + "ac")
		}
	default:
		return fmt.Errorf("Invalid 802.11 protocol")
	}

	log.Println("Using 802.11" + ap.ieee80211)

	if ap.channel == CHANNEL_AUTO && !ap.wifiIf.canAutoSelectChannel() {
		switch ap.ieee80211 {
		case "a", "ac":
			ap.channel = 36
		case "g":
			ap.channel = 1
		case "n":
			// 802.11n can be a 2.4 GHz or 5 GHz channel.
			// Use the first supported channel.
			ap.channel = ap.wifiIf.channels[0].num
		default:
			log.Panicln("Default channel for 802.11" + ap.ieee80211 + " is not set!")
		}
		log.Println("Your adapter does not support auto channel",
			"selection, using channel", ap.channel)
	}

	if ap.channel != CHANNEL_AUTO && !ap.wifiIf.canTransmitOnChannel(ap.channel) {
		return fmt.Errorf("Your adapter cannot transmit on channel %d", ap.channel)
	}

	ap.fatalError = make(chan error, 1)

	if hasNetworkManager() {
		log.Printf("Setting interface '%s' as unmanaged\n", ap.wifiIf.Name)

		err = networkManagerAddUnmanaged(ap.wifiIf.Name)
		if err != nil {
			return err
		}

		if networkManagerRunning() &&
			!networkManagerWaitUntilUnmanaged(ap.wifiIf.Name) {
			return fmt.Errorf("Failed to set interface '%s' as unmanaged", ap.wifiIf.Name)
		}
	}

	// Set IP on WiFi interface
	if err = ap.wifiIf.setDown(); err != nil {
		return err
	}

	if err = ap.wifiIf.addIPv4(ap.gateway); err != nil {
		return err
	}

	// Enable NAT
	if err = ap.initNAT(); err != nil {
		return err
	}

	// TODO: boost low entropy

	ap.confDir, err = ioutil.TempDir("", "create_ap."+ap.wifiIf.Name+".")
	if err != nil {
		return err
	}
	log.Println("Config dir:", ap.confDir)

	if err = ap.startDnsmasq(); err != nil {
		return err
	}

	if err = ap.startHostapd(); err != nil {
		return err
	}

	return nil
}

func (ap *AccessPoint) stop() {
	// Cleanup
	ap.deinitNAT()

	// Send termination signal to daemons
	for _, cmd := range ap.daemons {
		cmd.Process.Signal(syscall.SIGTERM)
	}

	// Sleep a bit until daemons are exited
	time.Sleep(300 * time.Millisecond)

	// If any of them are still alive, kill them.
	for _, cmd := range ap.daemons {
		cmd.Process.Signal(os.Kill)
	}

	if hasNetworkManager() {
		networkManagerRemoveUnmanaged(ap.wifiIf.Name)
	}

	if ap.confDir != "" {
		os.RemoveAll(ap.confDir)
	}
}

func (ap *AccessPoint) initNAT() error {
	err := runCmd("iptables", "-t", "nat", "-I", "POSTROUTING",
		"-s", ap.gateway.network().String(), "!", "-o", ap.wifiIf.Name, "-j", "MASQUERADE")
	if err != nil {
		return err
	}

	err = runCmd("iptables", "-I", "FORWARD", "-i", ap.wifiIf.Name,
		"!", "-o", ap.wifiIf.Name, "-j", "ACCEPT")
	if err != nil {
		return err
	}

	err = runCmd("iptables", "-I", "FORWARD", "-i", ap.wifiIf.Name,
		"-o", ap.wifiIf.Name, "-j", "ACCEPT")
	if err != nil {
		return err
	}

	err = ioutil.WriteFile("/proc/sys/net/ipv4/conf/all/forwarding", []byte("1"), 0644)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	if err != nil {
		return err
	}

	// To enable clients to establish PPTP connections we must
	// load nf_nat_pptp module.
	// If this command fails, we can continue without any problems.
	runCmd("modprobe", "nf_nat_pptp")

	return nil
}

func (ap *AccessPoint) deinitNAT() error {
	err := runCmd("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", ap.gateway.network().String(), "!", "-o", ap.wifiIf.Name, "-j", "MASQUERADE")
	if err != nil {
		return err
	}

	err = runCmd("iptables", "-D", "FORWARD", "-i", ap.wifiIf.Name,
		"!", "-o", ap.wifiIf.Name, "-j", "ACCEPT")
	if err != nil {
		return err
	}

	err = runCmd("iptables", "-D", "FORWARD", "-i", ap.wifiIf.Name,
		"-o", ap.wifiIf.Name, "-j", "ACCEPT")
	if err != nil {
		return err
	}

	return nil
}

func (ap *AccessPoint) configureDnsmasq() (string, error) {
	confFile := path.Join(ap.confDir, "dnsmasq.conf")

	f, err := os.OpenFile(confFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gateway := ap.gateway.IP.String()
	f.WriteString("listen-address=" + gateway + "\n")
	f.WriteString("bind-interfaces\n")
	f.WriteString(fmt.Sprintf("dhcp-range=%s,%s,%s,24h\n",
		ap.gateway.hostmin(), ap.gateway.hostmax(), ap.gateway.netmask()))
	f.WriteString("dhcp-option-force=option:router," + gateway + "\n")
	f.WriteString("dhcp-option-force=option:dns-server," + gateway + "\n")
	f.WriteString("dhcp-leasefile=" + path.Join(ap.confDir, "dnsmasq.leases") + "\n")
	f.WriteString("domain-needed\n")
	f.WriteString("localise-queries\n")

	return confFile, nil
}

func (ap *AccessPoint) startDnsmasq() error {
	log.Println("Starting dnsmasq")

	confFile, err := ap.configureDnsmasq()
	if err != nil {
		return err
	}

	// Allow ports 53 (tcp/udp) and 67 (udp)
	runCmd("iptables", "-I", "INPUT", "-p", "tcp", "-m", "tcp",
		"--dport", "53", "-j", "ACCEPT")
	runCmd("iptables", "-I", "INPUT", "-p", "udp", "-m", "udp",
		"--dport", "53", "-j", "ACCEPT")
	runCmd("iptables", "-I", "INPUT", "-p", "udp", "-m", "udp",
		"--dport", "67", "-j", "ACCEPT")

	// openSUSE's apparmor does not allow dnsmasq to read files.
	// Remove restriction.
	runCmd("complain", "dnsmasq")

	return ap.startCriticalDaemon("dnsmasq", "-k", "-C", confFile)
}

func (ap *AccessPoint) configureHostapd() (string, error) {
	var band uint

	confFile := path.Join(ap.confDir, "hostapd.conf")

	f, err := os.OpenFile(confFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	f.WriteString("interface=" + ap.wifiIf.Name + "\n")
	f.WriteString("bssid=" + ap.wifiIf.HardwareAddr.String() + "\n")
	f.WriteString("ssid=" + ap.ssid + "\n")
	f.WriteString(fmt.Sprintf("channel=%d\n", ap.channel))

	switch ap.ieee80211 {
	case "a":
		band = 5
	case "g":
		band = 2
	case "n":
		if ap.channel <= 14 {
			band = 2
		} else {
			band = 5
		}

		f.WriteString("ieee80211n=1\n")
		f.WriteString("wmm_enabled=1\n")
		// TODO: Improve ht_capab. Check OpenWRT.
		f.WriteString("ht_capab=[HT40+]\n")
	case "ac":
		band = 5
		f.WriteString("ieee80211n=1\n")
		f.WriteString("ieee80211ac=1\n")
		f.WriteString("wmm_enabled=1\n")
		f.WriteString("ht_capab=[HT40+]\n")
	}

	f.WriteString("country_code=" + ap.countryCode + "\n")
	f.WriteString("ieee80211d=1\n")

	switch band {
	case 2:
		f.WriteString("hw_mode=g\n")
	case 5:
		f.WriteString("hw_mode=a\n")
		// TODO: check if 802.11a support this
		f.WriteString("ieee80211h=1\n")
	}

	if len(ap.passphrase) > 0 {
		f.WriteString(fmt.Sprintf("wpa=%d\n", ap.wpa))
		if len(ap.passphrase) == 64 {
			f.WriteString("wpa_psk=" + ap.passphrase + "\n")
		} else {
			f.WriteString("wpa_passphrase=" + ap.passphrase + "\n")
		}
		f.WriteString("wpa_key_mgmt=WPA-PSK\n")
		f.WriteString("wpa_pairwise=TKIP CCMP\n")
		f.WriteString("rsn_pairwise=CCMP\n")
	}

	if ap.hiddenSSID {
		log.Println("SSID is hidden!")
		f.WriteString("ignore_broadcast_ssid=1\n")
	}

	if ap.isolateClients {
		log.Println("Clients will be isolated!")
		f.WriteString("ap_isolate=1\n")
	}

	f.WriteString("preamble=1\n")
	f.WriteString("beacon_int=100\n")
	f.WriteString("ctrl_interface=" + path.Join(ap.confDir, "hostapd_ctrl") + "\n")
	f.WriteString("ctrl_interface_group=0\n")
	f.WriteString("driver=nl80211\n")

	return confFile, nil
}

func (ap *AccessPoint) startHostapd() error {
	log.Println("Starting hostapd")

	confFile, err := ap.configureHostapd()
	if err != nil {
		return err
	}

	return ap.startCriticalDaemon("hostapd", confFile)
}

func (ap *AccessPoint) startCriticalDaemon(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		cmd.Wait()
		ap.fatalError <- fmt.Errorf("Critical daemon terminated (%s)", name)
	}()

	ap.daemons = append(ap.daemons, cmd)
	return nil

}
