package main

import (
	"log"
	"net"
	// "strconv"
	"sync"

	// "github.com/docker/docker/libnetwork/iptables"
	"github.com/docker/docker/libnetwork/netlabel"
	// "github.com/docker/docker/libnetwork/driverapi"
	"github.com/docker/docker/libnetwork/options"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
)

var (
	PLUGIN_NAME = "katharanp"
	PLUGIN_GUID = 0
)

// endpointConfiguration represents the user specified configuration for the sandbox endpoint
type endpointConfiguration struct {
	MacAddress net.HardwareAddr
}

type katharaEndpoint struct {
	macAddress  net.HardwareAddr
	vethInside  string
	vethOutside string
}

type katharaNetwork struct {
	ID                   string
	bridgeName string
	endpoints  map[string]*katharaEndpoint
	enableIPv6           bool

	// Internal fields set after ipam data parsing
	addressIPv4        *net.IPNet
	addressIPv6        *net.IPNet
	defaultGatewayIPv4 string
	defaultGatewayIPv6 string
	dbIndex            uint64
	dbExists           bool
}

type KatharaNetworkPlugin struct {
	scope    string
	networks map[string]*katharaNetwork
	sync.Mutex
}

func (k *KatharaNetworkPlugin) GetCapabilities() (*network.CapabilitiesResponse, error) {
	log.Printf("Received GetCapabilities req")

	capabilities := &network.CapabilitiesResponse{
		Scope: k.scope,
	}

	return capabilities, nil
}

func (k *KatharaNetworkPlugin) CreateNetwork(req *network.CreateNetworkRequest) error {
	log.Printf("Received CreateNetwork req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	if err := detectIpTables(); err != nil {
		log.Printf("CreateNetwork, detectIpTables error: %v", err)
		return err
	}

	if _, ok := k.networks[req.NetworkID]; ok {
		return types.ForbiddenErrorf("network %s exists", req.NetworkID)
	}

	// Parse the config.
	config, err := parseNetworkOptions(req.NetworkID, req.Options)
	if err != nil {
		return err
	}

	// Add IP addresses/gateways to the configuration.
	if err = config.processIPAM(req.NetworkID, req.IPv4Data, req.IPv6Data); err != nil {
		return err
	}

	// FixMe: Skip this if bridge did exist
	gatewayv4, ipnet, err := net.ParseCIDR(req.IPv4Data[0].Gateway)
	if err != nil {
		return err
	}
	log.Printf("CreateNetwork, gateway ip: %v, ipnet: %v", gatewayv4, ipnet)
	bridgeName, err := createBridge(req.NetworkID, req.IPv4Data[0].Gateway)
	if err != nil {
		log.Printf("CreateNetwork, createBridge error: %v", err)
		return err
	}
	log.Printf("CreateNetwork, created bridge: %v", bridgeName)

	/*
	gatewayv6 := ""
	if len(req.IPv6Data) > 0 {
		gatewayv6 = req.IPv6Data[0].Gateway
	}
	*/
	katharaNetwork := &katharaNetwork{
		ID: req.NetworkID,
		bridgeName: bridgeName,
		endpoints:  make(map[string]*katharaEndpoint),
		// defaultGatewayIPv4: gatewayv4.String(),
		// defaultGatewayIPv6: gatewayv6,
	}

	k.networks[req.NetworkID] = katharaNetwork

	return nil
}

func (k *KatharaNetworkPlugin) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	log.Printf("Received DeleteNetwork req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Skip if not in map */
	if _, ok := k.networks[req.NetworkID]; !ok {
		return nil
	}

	if err := detectIpTables(); err != nil {
		return err
	}

	err := deleteBridge(req.NetworkID)
	if err != nil {
		return err
	}

	delete(k.networks, req.NetworkID)

	return nil
}

func (k *KatharaNetworkPlugin) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	log.Printf("Received AllocateNetwork req:\n%+v\n", req)

	return nil, nil
}

func (k *KatharaNetworkPlugin) FreeNetwork(req *network.FreeNetworkRequest) error {
	log.Printf("Received FreeNetwork req:\n%+v\n", req)

	return nil
}

func (k *KatharaNetworkPlugin) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	log.Printf("Received CreateEndpoint req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error if not in map */
	if _, ok := k.networks[req.NetworkID]; !ok {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	intfInfo := new(network.EndpointInterface)

	if req.Options["kathara.mac_addr"] != nil {
		// Use a pre-defined MAC Address passed by the user
		intfInfo.MacAddress = req.Options["kathara.mac_addr"].(string)
	} else if req.Options["kathara.machine"] != nil && req.Options["kathara.iface"] != nil {
		// Generate the interface MAC Address by concatenating the machine name and the interface idx
		intfInfo.MacAddress = generateMacAddressFromID(req.Options["kathara.machine"].(string) + "-" + req.Options["kathara.iface"].(string))
	} else if req.Interface == nil {
		// Generate the interface MAC Address by concatenating the network id and the endpoint id
		intfInfo.MacAddress = generateMacAddressFromID(req.NetworkID + "-" + req.EndpointID)
	}

	parsedMac, _ := net.ParseMAC(intfInfo.MacAddress)

	/*
    // Extract the `com.docker.network.portmap` option
    portmapOption, ok := req.Options[netlabel.PortMap]
    if !ok {
		log.Printf("CreateEndpoint , Portmap option not found\n")
    }

    // Assert the type to a slice of maps
    portmaps, ok := portmapOption.([]interface{})
    if !ok {
        log.Println("CreateEndpoint, Invalid type for portmaps")
    }

    for _, portmapInterface := range portmaps {
        portmap, ok := portmapInterface.(map[string]interface{})
        if !ok {
            log.Println("CreateEndpoint, Invalid type for portmap entry")
            continue
        }

		proto, _ := portmap["Proto"]
		hostIP, _ := portmap["HostIP"]
        hostPort, _ := portmap["HostPort"]
        port, _ := portmap["Port"]

		if hostPort != port {
			return nil, types.ForbiddenErrorf("Target and source ports must be same")
		}
		if hostIP == nil {
			return nil, types.ForbiddenErrorf("Published IP address is required")
		}


		var publishRule = []string{"-p", strconv.FormatFloat(proto.(float64), 'f', -1, 64), "-d", hostIP.(string), "--dport", strconv.FormatFloat(hostPort.(float64), 'f', -1, 64), "-j", "ACCEPT"}
		var iptablev4 = iptables.GetIptable(iptables.IPv4)
		if err := iptablev4.ProgramRule(iptables.Filter, "FORWARD", iptables.Append, publishRule); err != nil {
			return nil, err
		}


		var lbNatRule = []string{"-p", strconv.FormatFloat(proto.(float64), 'f', -1, 64), "-d", hostIP.(string), "--dport", strconv.FormatFloat(hostPort.(float64), 'f', -1, 64), "-j", "DNAT"}
		var iptablev4 = iptables.GetIptable(iptables.IPv4)
		if err := iptablev4.ProgramRule(iptables.Filter, "DOCKER", iptables.Append, lbNatRule); err != nil {
			return nil, err
		}

    }
	*/

	endpoint := &katharaEndpoint{
		macAddress: parsedMac,
	}

	k.networks[req.NetworkID].endpoints[req.EndpointID] = endpoint

	resp := &network.CreateEndpointResponse{
		Interface: intfInfo,
	}

	return resp, nil
}

func (k *KatharaNetworkPlugin) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	log.Printf("Received DeleteEndpoint req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Skip if not in map (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil
	}

	delete(k.networks[req.NetworkID].endpoints, req.EndpointID)

	return nil
}

func (k *KatharaNetworkPlugin) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	log.Printf("Received EndpointOperInfo req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]
	value := make(map[string]string)

	value["ip_address"] = ""
	value["mac_address"] = endpointInfo.macAddress.String()
	value["veth_outside"] = endpointInfo.vethOutside

	resp := &network.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (k *KatharaNetworkPlugin) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	log.Printf("Received Join req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]
	vethInside, vethOutside, err := createVethPair(endpointInfo.macAddress)
	if err != nil {
		return nil, err
	}

	if err := attachInterfaceToBridge(k.networks[req.NetworkID].bridgeName, vethOutside); err != nil {
		return nil, err
	}

	k.networks[req.NetworkID].endpoints[req.EndpointID].vethInside = vethInside
	k.networks[req.NetworkID].endpoints[req.EndpointID].vethOutside = vethOutside

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   vethInside,
			DstPrefix: "eth",
		},
	}

	return resp, nil
}

func (k *KatharaNetworkPlugin) Leave(req *network.LeaveRequest) error {
	log.Printf("Received Leave req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]

	if err := deleteVethPair(endpointInfo.vethOutside); err != nil {
		return err
	}

	return nil
}

func (k *KatharaNetworkPlugin) DiscoverNew(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverNew req:\n%+v\n", req)

	return nil
}

func (k *KatharaNetworkPlugin) DiscoverDelete(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverDelete req:\n%+v\n", req)

	return nil
}

func (k *KatharaNetworkPlugin) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	log.Printf("Received ProgramExternalConnectivity req:\n%+v\n", req)

	return nil
}

func (k *KatharaNetworkPlugin) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	log.Printf("Received RevokeExternalConnectivity req:\n%+v\n", req)

	return nil
}

func NewKatharaNetworkPlugin(scope string, networks map[string]*katharaNetwork) (*KatharaNetworkPlugin, error) {
	katharanp := &KatharaNetworkPlugin{
		scope:    scope,
		networks: networks,
	}

	return katharanp, nil
}

func main() {
	driver, err := NewKatharaNetworkPlugin("local", map[string]*katharaNetwork{})

	if err != nil {
		log.Fatalf("ERROR: %s init failed!", PLUGIN_NAME)
	}

	requestHandler := network.NewHandler(driver)

	if err := requestHandler.ServeUnix(PLUGIN_NAME, PLUGIN_GUID); err != nil {
		log.Fatalf("ERROR: %s init failed!", PLUGIN_NAME)
	}
}


func parseNetworkOptions(id string, option options.Generic) (*katharaNetwork, error) {
	var (
		config = &katharaNetwork{}
	)


	// Process well-known labels next
	if val, ok := option[netlabel.EnableIPv6]; ok {
		config.enableIPv6 = val.(bool)
	}

	/*
	exists, err := bridgeInterfaceExists(config.bridgeName)
	if err != nil {
		return nil, err
	}

	if !exists {
		config.BridgeIfaceCreator = ifaceCreatedByLibnetwork
	} else {
		config.BridgeIfaceCreator = ifaceCreatedByUser
	}
	*/

	config.ID = id
	return config, nil
}

func (c *katharaNetwork) processIPAM(id string, ipamV4Data, ipamV6Data []*network.IPAMData) error {
	if len(ipamV4Data) > 1 || len(ipamV6Data) > 1 {
		return types.ForbiddenErrorf("bridge driver doesn't support multiple subnets")
	}

	if len(ipamV4Data) == 0 {
		return types.InvalidParameterErrorf("bridge network %s requires ipv4 configuration", id)
	}

	/*
	if ipamV4Data[0].Gateway != "" {
		c.addressIPv4 = types.GetIPNetCopy(ipamV4Data[0].Gateway)
	}


	if len(ipamV6Data) > 0 {
		c.addressIPv6 = ipamV6Data[0].Pool

		if ipamV6Data[0].Gateway != nil {
			c.addressIPv6 = types.GetIPNetCopy(ipamV6Data[0].Gateway)
		}
	}
	*/

	return nil
}
