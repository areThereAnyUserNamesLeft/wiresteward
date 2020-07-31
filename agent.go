package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Agent struct {
	device        string
	pubKey        string
	privKey       string
	netlinkHandle *netlinkHandle
	stop          chan bool
	tundev        *TunDevice
}

// NewAgent: Creates an agent associated with a net device
func NewAgent(deviceName string) (*Agent, error) {
	a := &Agent{
		device:        deviceName,
		netlinkHandle: NewNetLinkHandle(),
	}

	stop := make(chan bool)
	tundev, err := startTunDevice(deviceName, stop)
	if err != nil {
		return a, fmt.Errorf("Error starting wg device: %s: %v", deviceName, err)
	}

	a.stop = stop
	a.tundev = tundev

	go a.tundev.Run()

	// Bring device up
	if err := a.netlinkHandle.EnsureLinkUp(deviceName); err != nil {
		return a, err
	}

	// Check if there is a private key or generate one
	_, privKey, err := getKeys(deviceName)
	if err != nil {
		return a, fmt.Errorf("Cannot get keys for device: %s: %v", deviceName, err)
	}
	// the base64 value of an empty key will come as
	// AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
	if privKey == "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		newKey, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return a, err
		}
		a.privKey = newKey.String()
		if err := a.SetPrivKey(); err != nil {
			return a, err
		}
	}

	// Fetch keys from interface and save them
	a.pubKey, a.privKey, err = getKeys(deviceName)
	if err != nil {
		return a, err
	}

	return a, nil
}

func (a *Agent) requestWgConfig(serverUrl, token string) (*Response, error) {
	// Marshal key int json
	r, err := json.Marshal(&Request{PubKey: a.pubKey})
	if err != nil {
		return &Response{}, err
	}

	// Prepare the request
	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("%s/newPeerLease", serverUrl),
		bytes.NewBuffer(r),
	)
	req.Header.Set("Content-Type", "application/json")

	var bearer = "Bearer " + token
	req.Header.Set("Authorization", bearer)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return &Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return &Response{}, fmt.Errorf(
			"Response status: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return &Response{}, fmt.Errorf(
			"error reading response body: %s,", err.Error())
	}

	response := &Response{}
	if err := json.Unmarshal(body, response); err != nil {
		return response, err
	}

	return response, nil

}

func (a *Agent) SetPrivKey() error {
	return setPrivateKey(a.device, a.privKey)
}

func (a *Agent) addIpToDev(ip string) error {
	devIP, err := netlink.ParseIPNet(ip)
	if err != nil {
		return fmt.Errorf("Cannot parse offered ip net: %v", err)
	}
	log.Printf(
		"Configuring offered ip: %v on dev: %s\n",
		devIP,
		a.device,
	)
	if err := a.netlinkHandle.UpdateIP(a.device, devIP); err != nil {
		return err
	}
	return nil
}

func (a *Agent) addRoutesForAllowedIps(allowed_ips []string) error {
	for _, aip := range allowed_ips {
		dst, err := netlink.ParseIPNet(aip)
		if err != nil {
			return fmt.Errorf("Cannot parse ip: %s: %v", aip, err)
		}

		log.Printf("Adding route: %v on dev %s\n", dst, a.device)
		if err := a.netlinkHandle.AddRoute(a.device, dst); err != nil {
			return fmt.Errorf(
				"Eror adding route %v via %s: %v",
				dst,
				a.device,
				err,
			)
		}
	}
	return nil
}

// GetNewWgLease: talks to the peer server to ask for a new ip lease and
// and configures that ip on the related net interface. Returns the remote
// wireguard peer config and a list of allowed ips
func (a *Agent) GetNewWgLease(serverUrl string, token string) (*wgtypes.PeerConfig, []string, error) {
	resp, err := a.requestWgConfig(serverUrl, token)
	if err != nil {
		return &wgtypes.PeerConfig{}, []string{}, err
	}

	if err := a.addIpToDev(resp.IP); err != nil {
		return &wgtypes.PeerConfig{}, []string{}, err
	}

	allowed_ips := strings.Split(resp.AllowedIPs, ",")
	peer, err := newPeerConfig(resp.PubKey, "", resp.Endpoint, allowed_ips)
	if err != nil {
		return &wgtypes.PeerConfig{}, []string{}, err
	}

	return peer, allowed_ips, nil
}

func (a *Agent) Stop() {
	a.stop <- true
}
