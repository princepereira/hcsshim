package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/Microsoft/hcsshim/cmd/ncproxy/ncproxygrpc"
	"github.com/Microsoft/hcsshim/cmd/ncproxy/nodenetsvc"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// This is a barebones example of an implementation of the network
// config agent service that ncproxy talks to. This is solely used to test.

var (
	configPath = flag.String("config", "", "Path to JSON configuration file.")

	prefixLength = "24"
	ipVersion    = "4"
)

func generateMAC() (string, error) {
	buf := make([]byte, 6)

	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}

	// set first number to 0
	buf[0] = 0
	mac := net.HardwareAddr(buf)
	macString := strings.ToUpper(mac.String())
	macString = strings.Replace(macString, ":", "-", -1)

	return macString, nil
}

func generateIPs(prefixLength string) (string, string, string) {
	buf := []byte{192, 168, 50}

	// set last to 0 for prefix
	ipPrefixBytes := append(buf, 0)
	ipPrefix := net.IP(ipPrefixBytes)
	ipPrefixString := ipPrefix.String() + "/" + prefixLength

	// set the last to 1 for gateway
	ipGatewayBytes := append(buf, 1)
	ipGateway := net.IP(ipGatewayBytes)
	ipGatewayString := ipGateway.String()

	// set last byte for IP address in range
	last := byte(rand.Intn(255-2) + 2)
	ipBytes := append(buf, last)
	ip := net.IP(ipBytes)
	ipString := ip.String()

	return ipPrefixString, ipGatewayString, ipString
}

func (s *service) ConfigureContainerNetworking(ctx context.Context, req *nodenetsvc.ConfigureContainerNetworkingRequest) (_ *nodenetsvc.ConfigureContainerNetworkingResponse, err error) {
	// for testing purposes, make endpoints here
	log.G(ctx).WithField("req", req).Info("ConfigureContainerNetworking request")

	if req.RequestType == nodenetsvc.RequestType_Setup {
		prefixIP, gatewayIP, midIP := generateIPs(prefixLength)

		addNetworkReq := &ncproxygrpc.CreateNetworkRequest{
			Name:                  req.ContainerID + "_network",
			Mode:                  ncproxygrpc.CreateNetworkRequest_Transparent,
			SwitchName:            s.conf.NetworkingSettings.HNSSettings.SwitchName,
			IpamType:              ncproxygrpc.CreateNetworkRequest_Static,
			SubnetIpaddressPrefix: []string{prefixIP},
			DefaultGateway:        gatewayIP,
		}

		networkResp, err := s.client.CreateNetwork(ctx, addNetworkReq)
		if err != nil {
			return nil, err
		}

		network, err := hcn.GetNetworkByID(networkResp.ID)
		if err != nil {
			return nil, err
		}
		s.containerToNetwork[req.ContainerID] = network.Name

		mac, err := generateMAC()
		if err != nil {
			return nil, err
		}

		name := req.ContainerID + "_endpoint"
		endpointCreateReq := &ncproxygrpc.CreateEndpointRequest{
			Name:                  name,
			Macaddress:            mac,
			Ipaddress:             midIP,
			IpaddressPrefixlength: prefixLength,
			NetworkName:           network.Name,
			IovPolicySettings:     s.conf.NetworkingSettings.HNSSettings.IOVSettings,
		}

		endpt, err := s.client.CreateEndpoint(ctx, endpointCreateReq)
		if err != nil {
			return nil, err
		}

		log.G(ctx).WithField("endpt", endpt).Info("ConfigureContainerNetworking created endpoint")

		addEndpointReq := &ncproxygrpc.AddEndpointRequest{
			Name:        name,
			NamespaceID: req.NetworkNamespaceID,
		}
		_, err = s.client.AddEndpoint(ctx, addEndpointReq)
		if err != nil {
			return nil, err
		}
		s.containerToNamespace[req.ContainerID] = req.NetworkNamespaceID

		resultIPAddr := &nodenetsvc.ContainerIPAddress{
			Version:        ipVersion,
			Ip:             midIP,
			PrefixLength:   prefixLength,
			DefaultGateway: gatewayIP,
		}
		netInterface := &nodenetsvc.ContainerNetworkInterface{
			Name:               network.Name,
			MacAddress:         mac,
			NetworkNamespaceID: req.NetworkNamespaceID,
			Ipaddresses:        []*nodenetsvc.ContainerIPAddress{resultIPAddr},
		}

		return &nodenetsvc.ConfigureContainerNetworkingResponse{
			Interfaces: []*nodenetsvc.ContainerNetworkInterface{netInterface},
		}, nil
	} else if req.RequestType == nodenetsvc.RequestType_Teardown {
		eReq := &ncproxygrpc.GetEndpointsRequest{}
		resp, err := s.client.GetEndpoints(ctx, eReq)
		if err != nil {
			return nil, err
		}

		for _, endpoint := range resp.Endpoints {
			if endpoint.Namespace == req.NetworkNamespaceID {
				deleteEndptReq := &ncproxygrpc.DeleteEndpointRequest{
					Name: endpoint.Name,
				}
				if _, err := s.client.DeleteEndpoint(ctx, deleteEndptReq); err != nil {
					log.G(ctx).WithField("name", endpoint.Name).Warn("failed to delete endpoint")
				}
			}
		}

		if networkName, ok := s.containerToNetwork[req.ContainerID]; ok {
			deleteReq := &ncproxygrpc.DeleteNetworkRequest{
				Name: networkName,
			}
			if _, err := s.client.DeleteNetwork(ctx, deleteReq); err != nil {
				log.G(ctx).WithField("name", networkName).Warn("failed to delete network")
			}
			delete(s.containerToNetwork, req.ContainerID)
		}

		return &nodenetsvc.ConfigureContainerNetworkingResponse{}, nil

	}
	return nil, fmt.Errorf("invalid request type %v", req.RequestType)
}

func (s *service) addHelper(ctx context.Context, req *nodenetsvc.ConfigureNetworkingRequest, containerNamespaceID string) (_ *nodenetsvc.ConfigureNetworkingResponse, err error) {
	return s.addHNSHelper(ctx, req, containerNamespaceID)
}

func (s *service) addHNSHelper(ctx context.Context, req *nodenetsvc.ConfigureNetworkingRequest, containerNamespaceID string) (_ *nodenetsvc.ConfigureNetworkingResponse, err error) {
	eReq := &ncproxygrpc.GetEndpointsRequest{}
	resp, err := s.client.GetEndpoints(ctx, eReq)
	if err != nil {
		return nil, err
	}
	log.G(ctx).WithField("endpts", resp.Endpoints).Info("ConfigureNetworking addrequest")

	for _, endpoint := range resp.Endpoints {
		if endpoint.Namespace == containerNamespaceID {
			// add endpoints that are in the namespace as NICs
			nicID, err := guid.NewV4()
			if err != nil {
				return nil, fmt.Errorf("failed to create nic GUID: %s", err)
			}
			nsReq := &ncproxygrpc.AddNICRequest{
				ContainerID:  req.ContainerID,
				NicID:        nicID.String(),
				EndpointName: endpoint.Name,
			}
			if _, err := s.client.AddNIC(ctx, nsReq); err != nil {
				return nil, err
			}
			s.endpointToNicID[endpoint.Name] = nicID.String()
		}

	}

	defer func() {
		if err != nil {
			_, _ = s.teardownHelper(ctx, req, containerNamespaceID)
		}
	}()

	return &nodenetsvc.ConfigureNetworkingResponse{}, nil

}

func (s *service) teardownHelper(ctx context.Context, req *nodenetsvc.ConfigureNetworkingRequest, containerNamespaceID string) (*nodenetsvc.ConfigureNetworkingResponse, error) {
	eReq := &ncproxygrpc.GetEndpointsRequest{}
	resp, err := s.client.GetEndpoints(ctx, eReq)
	if err != nil {
		return nil, err
	}
	for _, endpoint := range resp.Endpoints {
		if endpoint.Namespace == containerNamespaceID {
			nicID, ok := s.endpointToNicID[endpoint.Name]
			if !ok {
				log.G(ctx).WithField("name", endpoint.Name).Warn("endpoint was not assigned a NIC ID previously")
				continue
			}
			// remove endpoints that are in the namespace as NICs
			nsReq := &ncproxygrpc.DeleteNICRequest{
				ContainerID:  req.ContainerID,
				NicID:        nicID,
				EndpointName: endpoint.Name,
			}
			if _, err := s.client.DeleteNIC(ctx, nsReq); err != nil {
				log.G(ctx).WithField("name", endpoint.Name).Warn("failed to delete endpoint nic")
			}
			delete(s.endpointToNicID, endpoint.Name)
		}
	}
	return &nodenetsvc.ConfigureNetworkingResponse{}, nil
}

func (s *service) ConfigureNetworking(ctx context.Context, req *nodenetsvc.ConfigureNetworkingRequest) (*nodenetsvc.ConfigureNetworkingResponse, error) {
	log.G(ctx).WithField("req", req).Info("ConfigureNetworking request")

	containerNamespaceID, ok := s.containerToNamespace[req.ContainerID]
	if !ok {
		return nil, fmt.Errorf("no namespace was previously created for containerID %s", req.ContainerID)
	}

	if req.RequestType == nodenetsvc.RequestType_Setup {
		return s.addHelper(ctx, req, containerNamespaceID)
	}
	return s.teardownHelper(ctx, req, containerNamespaceID)
}

// GetHostLocalIpAddress is defined in the nodenetworksvc proto while is owned by the azure vnetagent team
//nolint:stylecheck
func (s *service) GetHostLocalIpAddress(ctx context.Context, req *nodenetsvc.GetHostLocalIpAddressRequest) (*nodenetsvc.GetHostLocalIpAddressResponse, error) {
	return &nodenetsvc.GetHostLocalIpAddressResponse{IpAddr: ""}, nil
}

func (s *service) PingNodeNetworkService(ctx context.Context, req *nodenetsvc.PingNodeNetworkServiceRequest) (*nodenetsvc.PingNodeNetworkServiceResponse, error) {
	return &nodenetsvc.PingNodeNetworkServiceResponse{}, nil
}

func main() {
	var err error
	ctx := context.Background()

	flag.Parse()
	conf, err := readConfig(*configPath)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to read network agent's config file at %s", *configPath)
	}
	log.G(ctx).WithFields(logrus.Fields{
		"config path": *configPath,
		"conf":        conf,
	}).Info("network agent configuration")

	sigChan := make(chan os.Signal, 1)
	serveErr := make(chan error, 1)
	defer close(serveErr)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	grpcClient, err := grpc.Dial(
		conf.GRPCAddr,
		grpc.WithInsecure(),
	)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to connect to ncproxy at %s", conf.GRPCAddr)
	}
	defer grpcClient.Close()

	log.G(ctx).WithField("addr", conf.GRPCAddr).Info("connected to ncproxy")
	ncproxyClient := ncproxygrpc.NewNetworkConfigProxyClient(grpcClient)
	service := &service{
		conf:                 conf,
		client:               ncproxyClient,
		containerToNamespace: make(map[string]string),
		endpointToNicID:      make(map[string]string),
		containerToNetwork:   make(map[string]string),
	}
	server := grpc.NewServer()
	nodenetsvc.RegisterNodeNetworkServiceServer(server, service)

	grpcListener, err := net.Listen("tcp", conf.NodeNetSvcAddr)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to listen on %s", grpcListener.Addr().String())
	}

	go func() {
		defer grpcListener.Close()
		if err := server.Serve(grpcListener); err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				serveErr <- nil
			}
			serveErr <- err
		}
	}()

	log.G(ctx).WithField("addr", conf.NodeNetSvcAddr).Info("serving network service agent")

	// Wait for server error or user cancellation.
	select {
	case <-sigChan:
		log.G(ctx).Info("Received interrupt. Closing")
	case err := <-serveErr:
		if err != nil {
			log.G(ctx).WithError(err).Fatal("grpc service failure")
		}
	}

	// Cancel inflight requests and shutdown service
	server.GracefulStop()
}
