package cloudprovider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	novaservers "github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	neutronports "github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	neutronsubnets "github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// PlatformTypeOpenStack is the string representation for the OpenStack platform type.
	PlatformTypeOpenStack   = "OpenStack"
	openstackCloudName      = "openstack"
	openstackProviderPrefix = "openstack:///"
	egressIPTag             = "OpenShiftEgressIP"
	novaDeviceOwner         = "compute:nova"

	// NOTE: Capacity is defined on a per interface basis as:
	// - IP address capacity for each node, where the capacity is either IP family
	//   agnostic or not.
	// However, in OpenStack, we do not have such a thing as an interface capacity.
	// Therefore, we settle on a sane ceiling of 64 IP addresses per interface.
	// Should customers run into this ceiling, there should be no issue to raise it
	// in the future.
	openstackMaxCapacity = 64
)

// OpenStack implements the API wrapper for talking
// to the OpenStack API
type OpenStack struct {
	CloudProvider
	novaClient    *gophercloud.ServiceClient
	neutronClient *gophercloud.ServiceClient
}

// initCredentials initializes the cloud API credentials by reading the
// secret data which has been mounted in cloudProviderSecretLocation. The
// mounted secret data in Kubernetes is generated following a one-to-one
// mapping between each .data field and a corresponding file.
// For OpenStack, read the generated clouds.yaml file inside
// cloudProviderSecretLocation for auth purposes.
func (o *OpenStack) initCredentials() error {
	var err error

	// Read the clouds.yaml file.
	// That information is stored in secret cloud-credentials.
	clientConfigFile := filepath.Join(o.cfg.CredentialDir, "clouds.yaml")
	content, err := ioutil.ReadFile(clientConfigFile)
	if err != nil {
		return fmt.Errorf("could read file %s, err: %q", clientConfigFile, err)
	}

	// Unmarshal YAML content into Clouds object.
	var clouds clientconfig.Clouds
	err = yaml.Unmarshal(content, &clouds)
	if err != nil {
		return fmt.Errorf("could not parse cloud configuration from %s, err: %q", clientConfigFile, err)
	}
	// We expect that the cloud in clouds.yaml be named "openstack".
	cloud, ok := clouds.Clouds[openstackCloudName]
	if !ok {
		return fmt.Errorf("invalid clouds.yaml file. Missing section for cloud name '%s'", openstackCloudName)
	}

	// Set AllowReauth to enable reauth when the token expires. Otherwise, we'll get endless ""Authentication failed"
	// errors after the token expired.
	// https://github.com/gophercloud/gophercloud/blob/a5d8e32ad107b1b72635a2e823ddd6c28fa0d4e7/auth_options.go#L70
	// https://github.com/gophercloud/gophercloud/blob/513734676e6495f6fec60e7aaf1f86f1ce807428/openstack/client.go#L151
	cloud.AuthInfo.AllowReauth = true

	// Prepare the options.
	clientOpts := &clientconfig.ClientOpts{
		Cloud:      cloud.Cloud,
		AuthType:   cloud.AuthType,
		AuthInfo:   cloud.AuthInfo,
		RegionName: cloud.RegionName,
	}
	opts, err := clientconfig.AuthOptions(clientOpts)
	if err != nil {
		return err
	}
	provider, err := openstack.NewClient(opts.IdentityEndpoint)
	if err != nil {
		return err
	}

	// Read CA information - needed for self-signed certificates.
	// That information is stored in ConfigMap kube-cloud-config.
	caBundle := filepath.Join(o.cfg.ConfigDir, "ca-bundle.pem")
	userCACert, err := ioutil.ReadFile(caBundle)
	if err == nil && string(userCACert) != "" {
		klog.Infof("Custom CA bundle found at location '%s' - reading certificate information", caBundle)
		certPool, err := x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("could not initialize x509 SystemCertPool, err: %q", err)
		}
		transport := http.Transport{}
		certPool.AppendCertsFromPEM([]byte(userCACert))
		transport.TLSClientConfig = &tls.Config{RootCAs: certPool}
		provider.HTTPClient = http.Client{Transport: &transport}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not parse file '%s', err: %q", caBundle, err)
	} else {
		klog.Infof("Could not find custom CA bundle in file '%s' - some environments require a custom CA to work correctly", caBundle)
	}

	// Now, authenticate.
	err = openstack.Authenticate(provider, *opts)
	if err != nil {
		return err
	}

	// And create a client for nova (compute / servers).
	o.novaClient, err = openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		//	Region: cloud.RegionName,
	})
	if err != nil {
		return err
	}

	// And another client for neutron (network).
	o.neutronClient, err = openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		//	Region: cloud.RegionName,
	})
	if err != nil {
		return err
	}

	return nil
}

func (o *OpenStack) findAssignSubnetAndPort(ip net.IP, node *corev1.Node) (*neutronsubnets.Subnet, *neutronports.Port, error) {
	// List all ports that are attached to this server.
	serverID, err := getNovaServerIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, nil, err
	}
	serverPorts, err := o.listNovaServerPorts(serverID)
	if err != nil {
		return nil, nil, err
	}

	// Loop over all ports that are attached to this nova instance and find the subnets
	// that are attached to the port's network.
	for _, serverPort := range serverPorts {
		// If this IP address is already allowed on the port (speak: part of allowed_address_pairs),
		// then return an AlreadyExistingIPError and skip all further steps.
		if isIPAddressAllowedOnNeutronPort(serverPort, ip) {
			// This is part of normal operation.
			// Callers will likely ignore this and go on with their business logic and
			// report success to the user.
			return nil, nil, AlreadyExistingIPError
		}

		// Get all subnets that are attached to this port.
		subnets, err := o.getNeutronSubnetsForNetwork(serverPort.NetworkID)
		if err != nil {
			klog.Warningf("Could not find subnet information for network %s, err: %q", serverPort.NetworkID, err)
			continue
		}
		// 1) Loop over all subnets of the port and check if the IP address fits inside the subnet CIDR.
		// If the IP address is inside the subnet:
		//   2) Reserve the IP address on the subnet by creating a new unattached neutron port.
		//      Set variable unboundPort, and exit out of the subnet loop.
		//   3) Then, add the IP address to the port's allowed_address_pairs.
		//   4) Return nil to indicate success if steps 2 and 3 passed.
		// 5) Throw an error if the IP address does not fit in any of the attached network's subnets.
		var matchingSubnet *neutronsubnets.Subnet
		for _, s := range subnets {
			// Because we're dealing with a pointer here for matchingSubnet:
			// we must reassign s:= s or we'd overwrite the content that we point
			// to.
			s := s
			// 1) Loop over all subnets and check if the IP address matches the subnet CIDR. If the IP
			//    addresses matches multiple subnets on the same server port, then something is wrong
			//    with this server's configuration and we should refuse to continue by throwing an error.
			_, ipnet, err := net.ParseCIDR(s.CIDR)
			if err != nil {
				klog.Warningf("Could not parse subnet information %s for network %s, err: %q",
					s.CIDR, serverPort.NetworkID, err)
				continue
			}
			if !ipnet.Contains(ip) {
				continue
			}
			if matchingSubnet != nil {
				return nil, nil, fmt.Errorf("requested IP address %s for node %s and port %s matches 2 different subnets, %s and %s",
					ip, node.Name, serverPort.ID, matchingSubnet.ID, s.ID)
			}

			matchingSubnet = &s
		}

		if matchingSubnet != nil {
			return matchingSubnet, &serverPort, nil
		}
	}

	// 5) The IP address does not fit in any of the attached networks' subnets.
	return nil, nil, fmt.Errorf("could not assign IP address %s to node %s", ip, node.Name)
}

// AssignPrivateIP attempts to assigning the IP address provided to the VM
// instance corresponding to the corev1.Node provided on the cloud the
// cluster is deployed on.
// NOTE: This operation is performed against all interfaces that are attached
// to the server. In case that an instance has 2 interfaces with the same CIDR
// that this IP address could fit in, the first interface that is found will be used.
// No guarantees about the correct interface ordering are given in such a case.
// Throw an AlreadyExistingIPError if the IP provided is already associated with the
// node, it's up to the caller to decide what to do with that.
// NOTE: For OpenStack, this is a 2 step operation which is not atomic:
//   a) Reserve a neutron port.
//   b) Add the IP address to the allowed_address_pairs field.
// If step b) fails, then we will try to undo step a). However, if this undo fails,
// then we will be in a situation where the user or an upper layer will have to call
// ReleasePrivateIP to get out of this situation.
func (o *OpenStack) AssignPrivateIP(ip net.IP, node *corev1.Node) error {
	if node == nil {
		return fmt.Errorf("invalid nil pointer provided for node when trying to assign private IP %s", ip.String())
	}
	// List all ports that are attached to this server.
	serverID, err := getNovaServerIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return err
	}

	matchingSubnet, matchingPort, err := o.findAssignSubnetAndPort(ip, node)
	if err != nil {
		return err
	}

	if matchingSubnet != nil {
		// 2) Reserve the IP address on the subnet by creating a new unattached neutron port.
		unboundPort, err := o.reserveNeutronIPAddress(*matchingSubnet, ip, serverID)
		if err != nil {
			return err
		}
		// 3) Then, add the IP address to the port's allowed_address_pairs.
		//    TODO: use a more elegant retry mechanism.
		if err = o.allowIPAddressOnNeutronPort(matchingPort.ID, ip); err != nil && !errors.Is(err, AlreadyExistingIPError) {
			// Try to clean up the allocated port if adding the IP to allowed_address_pairs failed.
			// Try this 10 times, but if this operation fails more than that, then user intervention is needed or
			// the upper layer must call ReleasePrivateIP (because if the neutron port exists and holds
			// a reservation, then the assign step will not continue after step 2).
			var errRelease error
			var releaseStatus string
			for i := 0; i < 10; i++ {
				errRelease = o.releaseNeutronIPAddress(*unboundPort, serverID)
				// If the release operation was successful, then we are done.
				if errRelease == nil {
					releaseStatus = "Released neutron port reservation."
					break
				}
				// Otherwise store the error message and retry.
				releaseStatus = fmt.Sprintf("Could not release neutron port reservation after %d tries, err: %q", i+1, errRelease)
			}
			return fmt.Errorf("could not allow IP address %s on port %s, err: %q. %s", ip.String(), matchingPort.ID, err, releaseStatus)
		}
		// 4) Return nil to indicate success if steps 2 and 3 passed.
		return nil
	}

	// 5) The IP address does not fit in any of the attached networks' subnets.
	return fmt.Errorf("could not assign IP address %s to node %s", ip, node.Name)
}

func (o *OpenStack) AllowsMovePrivateIP() bool {
	return true
}

func (o *OpenStack) MovePrivateIP(ip net.IP, nodeToAdd, nodeToDel *corev1.Node) error {
	if nodeToAdd == nil || nodeToDel == nil {
		return fmt.Errorf("invalid nil pointer provided for node when trying to move IP %s", ip.String())
	}

	// List all ports that are attached to this server.
	serverID, err := getNovaServerIDFromProviderID(nodeToDel.Spec.ProviderID)
	if err != nil {
		return err
	}
	serverPorts, err := o.listNovaServerPorts(serverID)
	if err != nil {
		return err
	}

	// Loop over all ports that are attached to this nova instance.
	for _, serverPort := range serverPorts {
		if isIPAddressAllowedOnNeutronPort(serverPort, ip) {
			if err = o.unallowIPAddressOnNeutronPort(serverPort.ID, ip); err != nil {
				return err
			}
		}
	}

	// TODO(dulek): Should we even care if we haven't found the IP? I'd say no, maybe we've removed it in
	//              a previous try?

	_, port, err := o.findAssignSubnetAndPort(ip, nodeToAdd)
	if err != nil {
		return err
	}

	if err = o.allowIPAddressOnNeutronPort(port.ID, ip); err != nil && !errors.Is(err, AlreadyExistingIPError) {
		return fmt.Errorf("could not allow IP address %s on port %s, err: %q", ip.String(), port.ID, err)
	}
	return nil
}

// ReleasePrivateIP attempts to release the IP address provided from the
// VM instance corresponding to the corev1.Node provided on the cloud the
// cluster is deployed on.
// ReleasePrivateIP must be idempotent, meaning that it will release
// all matching IP allowed_address_pairs for ports which are bound to this server.
// It also means that any unbound port on any network that is attached to this server -
// having the IP address to be released and matching the correct DeviceOwner and DeviceID
// containing the serverID will be deleted, as well.
// In OpenStack, it is possible to create different subnets with the exact same CIDR.
// These different subnets can then be assigned to ports on the same server.
// Hence, a server could be connected to several ports where the same IP is part of the
// allowed_address_pairs and where the same IP is reserved in neutron.
// NOTE: If the IP is non-existant: it returns an NonExistingIPError. The caller will
// likely want to ignore such an error and continue its normal operation.
func (o *OpenStack) ReleasePrivateIP(ip net.IP, node *corev1.Node) error {
	if node == nil {
		return fmt.Errorf("invalid nil pointer provided for node when trying to release IP %s", ip.String())
	}
	// List all ports that are attached to this server.
	serverID, err := getNovaServerIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return err
	}
	serverPorts, err := o.listNovaServerPorts(serverID)
	if err != nil {
		return err
	}

	// Loop over all ports that are attached to this nova instance.
	isFound := false
	for _, serverPort := range serverPorts {
		// 1) Check if the IP address is part of the port's allowed_address_pairs.
		//   If that's the case:
		//     a) Remove the IP address from the port's allowed_address_pairs.
		// 2) Loop over all subnets that are attached to this port and check if the
		//    IP address is inside the subnet.
		//    a) Does the IP address fit inside the given subnet? This verification can safe
		//       needless calls to the neutron API.
		//       b) If so, check if the the IP address is inside the subnet.
		//          c) If so, release the IP allocation = delete the unbound neutron port inside the subnet.
		// 3) The IP address is not part of any attached subnet and it's not part of any allowed_address_pair
		// on any of the ports that are attached to the server. In that case, return a NonExistingIPError.
		// This is part of normal operation and upper layers should ignore this error and go on with normal
		// business logic.
		// Mind that if 1) fails and returns an error, then this method will return the error and
		// the operation will be retried. Should 2) fail and return an error, then this method will
		// return the error and the operation will be retried. The next time, the first operation
		// will be skipped and only the second operation will be run. In a worst case scenario,
		// if the last operation fails continuously, we will end up with a dangling unbound neutron
		// port that must be deleted manually.

		// 1) Check if the IP address is part of the port's allowed_address_pairs.
		if isIPAddressAllowedOnNeutronPort(serverPort, ip) {
			isFound = true
			// 1) a) Remove the IP address from the port's allowed_address_pairs.
			if err = o.unallowIPAddressOnNeutronPort(serverPort.ID, ip); err != nil {
				return err
			}
		}

		// 2) Get all subnets that are attached to this port's network and search for the neutron port
		// holding the IP address.
		subnets, err := o.getNeutronSubnetsForNetwork(serverPort.NetworkID)
		if err != nil {
			klog.Warningf("Could not find subnet information for network %s, err: %q", serverPort.NetworkID, err)
			continue
		}
		for _, s := range subnets {
			// 2) a) Does the IP address fit inside the given subnet? This verification can save
			// needless calls to the neutron API.
			_, ipnet, err := net.ParseCIDR(s.CIDR)
			if err != nil {
				klog.Warningf("Could not parse subnet information %s for network %s, err: %q",
					s.CIDR, serverPort.NetworkID, err)
				continue
			}
			if !ipnet.Contains(ip) {
				continue
			}
			// 2) b) Is the IP address on the subnet?
			// The DeviceOwner and DeviceID that this is a port that identify that this is managed by this plugin.
			if unboundPort, err := o.getNeutronPortWithIPAddressAndMachineID(s, ip, serverID); err == nil {
				isFound = true
				// 2) c)  Then, release the IP allocation = delete the unbound neutron port.
				if err = o.releaseNeutronIPAddress(*unboundPort, serverID); err != nil {
					return err
				}
				// We could break here now. However, go on here with the next subnet on this port
				// to cover the very odd case that 2 subnets with the same CIDR were attached to the same
				// node port and that for some reason both subnets had a port reservation with the correct
				// DeviceOwner/DeviceID.
				// break  // omitted on purpose
			}
		}
	}
	// 3) The IP address is not part of any attached subnet and it's not part of any allowed_address_pair
	// on any of the ports that are attached to the server.
	if !isFound {
		// This is part of normal operation.
		// Callers will likely ignore this and go on with normal operation.
		return NonExistingIPError
	}

	return nil
}

// GetNodeEgressIPConfiguration retrieves the egress IP configuration for
// the node, following the convention the cloud uses. This means
// specifically for OpenStack:
// * The IP capacity is limited by the size of the subnet as well as the current
// neutron quotas.
// * The interface is keyed by a neutron UUID
// This function should only be called when no egress IPs have been added to the node,
// it will return an incorrect "egress IP capacity" otherwise.
func (o *OpenStack) GetNodeEgressIPConfiguration(node *corev1.Node) ([]*NodeEgressIPConfiguration, error) {
	if node == nil {
		return nil, fmt.Errorf("invalid nil pointer provided for node when trying to get node EgressIP configuration")
	}

	var configurations []*NodeEgressIPConfiguration

	serverID, err := getNovaServerIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}
	serverPorts, err := o.listNovaServerPorts(serverID)
	if err != nil {
		return nil, err
	}

	// For each port, generate one entry in the slice of NodeEgressIPConfigurations.
	// Add a sanity check: do not allow the same CIDR to be attached to 2 different ports,
	// otherwise we don't know where the EgressIP should be attached to.
	cidrs := make(map[string]struct{})
	for _, p := range serverPorts {
		// Retrieve configuration for this port.
		config, err := o.getNeutronPortNodeEgressIPConfiguration(p)
		if err != nil {
			return nil, err
		}

		// Check for duplicate CIDR assignments.
		if config.IFAddr.IPv4 != "" {
			if _, ok := cidrs[config.IFAddr.IPv4]; ok {
				return nil, fmt.Errorf("IPv4 CIDR '%s' is attached more than once to node %s", config.IFAddr.IPv4, node.Name)
			}
			cidrs[config.IFAddr.IPv4] = struct{}{}
		}
		if config.IFAddr.IPv6 != "" {
			if _, ok := cidrs[config.IFAddr.IPv6]; ok {
				return nil, fmt.Errorf("IPv6 CIDR '%s' is attached more than once to node %s", config.IFAddr.IPv6, node.Name)
			}
			cidrs[config.IFAddr.IPv6] = struct{}{}
		}

		// Append configuration to list of configurations.
		configurations = append(configurations, config)
	}

	return configurations, nil
}

// getNeutronPortNodeEgressIPConfiguration renders the NeutronPortNodeEgressIPConfiguration for a given port.
// * The interface is keyed by a neutron UUID
// * If multiple IPv4 repectively multiple IPv6 subnets are attached to the same port, throw an error.
// * The IP capacity is per port, per IP address family. It's ceiling is limited by the maximum of:
//   a) The size of the subnet.
//   b) An arbitrarily selected ceiling of 64.
//   The number of unique IP addresses in allowed_address_pair and fixed_ips is subtracted from that ceiling.
//   Keep in mind that the subnet size is an upper limit for the subnet, the port quota an upper limit for the
//   project but that IP capacity is a per port value.
//   The definition of this field does unfortunately not play very well with the way how neutron operates as there
//   is no such thing as a per port quota or limit.
// TODO: How to determine the primary interface of an instance if multiple interfaces are attached?
// TODO: As a solution, we currently report the EgressIP configuration for every attached interface, but other plugins
// do not do this. Is the upper layer compatible with that?
// TODO: How to determine the primary AF?
func (o *OpenStack) getNeutronPortNodeEgressIPConfiguration(p neutronports.Port) (*NodeEgressIPConfiguration, error) {
	var ipv4, ipv6 string
	var ipv4Prefix, ipv6Prefix int
	var ipv4Cap, ipv6Cap int
	var err error
	var ip net.IP
	var ipnet *net.IPNet

	// Retrieve all subnets for this port.
	subnets, err := o.getNeutronSubnetsForNetwork(p.NetworkID)
	if err != nil {
		return nil, fmt.Errorf("could not find subnet information for network %s, err: %q", p.NetworkID, err)
	}

	// Loop over all subnets. OpenStack potentially has several IPv4 or IPv6 subnets per port, but the
	// CloudPrivateIPConfig expects only a single subnet of each address family per port. Throw an error
	// in such a case.
	for _, s := range subnets {
		// Parse CIDR information into ip and ipnet.
		ip, ipnet, err = net.ParseCIDR(s.CIDR)
		if err != nil {
			return nil, fmt.Errorf("could not parse subnet information %s for network %s, err: %q",
				s.CIDR, p.NetworkID, err)
		}
		// For IPv4 and IPv6, calculate the capacity.
		if utilnet.IsIPv4(ip) {
			if ipv4 != "" {
				return nil, fmt.Errorf("found multiple IPv4 subnets attached to port %s, this is not supported", p.ID)
			}
			ipv4 = ipnet.String()
			ipv4Prefix, _ = ipnet.Mask.Size()
			ipv4Cap = int(math.Min(float64(openstackMaxCapacity), math.Pow(2, 32-float64(ipv4Prefix))-2))
		} else {
			if ipv6 != "" {
				return nil, fmt.Errorf("found multiple IPv6 subnets attached to port %s, this is not supported", p.ID)
			}
			ipv6 = ipnet.String()
			ipv6Prefix, _ = ipnet.Mask.Size()
			ipv6Cap = int(math.Min(float64(openstackMaxCapacity), math.Pow(2, 128-float64(ipv6Prefix))-2))
		}

	}

	ipv4UsedIPs, ipv6UsedIPs := o.getIPsOnPort(p)

	return &NodeEgressIPConfiguration{
		Interface: p.ID,
		IFAddr: ifAddr{
			IPv4: ipv4,
			IPv6: ipv6,
		},
		Capacity: capacity{
			IPv4: ipv4Cap - ipv4UsedIPs,
			IPv6: ipv6Cap - ipv6UsedIPs,
		},
	}, nil
}

// getIPsOnPort returns the number of unique IP addresses on the given port.
// Those include both IPs in the list of FixedIP addresses and in the list of
// allowed_address_pairs. This method is a helper for getNeutronPortNodeEgressIPConfiguration
// and as such does not test if multiple networks of the same address family are assigned to
// the port, given that the calling method will already have done this.
func (o *OpenStack) getIPsOnPort(p neutronports.Port) (int, int) {
	ipv4UsedIPs := make(map[string]struct{})
	ipv6UsedIPs := make(map[string]struct{})

	for _, ip := range p.FixedIPs {
		if utilnet.IsIPv4(net.ParseIP(ip.IPAddress)) {
			ipv4UsedIPs[ip.IPAddress] = struct{}{}
		} else {
			ipv6UsedIPs[ip.IPAddress] = struct{}{}
		}
	}
	for _, ip := range p.AllowedAddressPairs {
		if utilnet.IsIPv4(net.ParseIP(ip.IPAddress)) {
			ipv4UsedIPs[ip.IPAddress] = struct{}{}
		} else {
			ipv6UsedIPs[ip.IPAddress] = struct{}{}
		}
	}

	return len(ipv4UsedIPs), len(ipv6UsedIPs)
}

// reserveNeutronIPAddress creates a new unattached neutron port with the given IP on
// the given subnet. This will serve as our IPAM as it is impossible to create 2 ports
// with the same IP on the same subnet. The created port will be identified with a custom
// DeviceID and DeviceOwner.
// NOTE: We are not using tags. According to the neutron API, it's possible to add a tag when creating
// a port. But gophercloud does not allow us to do that and we must use a 2 step process (create port, then
// add tag).
func (o *OpenStack) reserveNeutronIPAddress(s neutronsubnets.Subnet, ip net.IP, serverID string) (*neutronports.Port, error) {
	if serverID == "" || len(serverID) > 254-len(egressIPTag) {
		return nil, fmt.Errorf("cannot assign IP address %s on subnet %s with an invalid serverID '%s'", ip.String(), s.ID, serverID)
	}

	// Now, create the port.
	opts := neutronports.CreateOpts{
		NetworkID: s.NetworkID,
		FixedIPs: []neutronports.IP{
			{
				SubnetID:  s.ID,
				IPAddress: ip.String(),
			},
		},
		DeviceOwner: egressIPTag,
		DeviceID:    generateDeviceID(serverID),
		Name:        fmt.Sprintf("egressip-%s", ip.String()),
	}
	p, err := neutronports.Create(o.neutronClient, opts).Extract()
	if err != nil {
		return nil, err
	}

	return p, nil
}

// releaseNeutronIPAddress deletes an unattached neutron port with the given IP on
// the given subnet. It also looks at the DeviceOwner and DeviceID and makes sure that the port matches.
func (o *OpenStack) releaseNeutronIPAddress(port neutronports.Port, serverID string) error {
	if serverID == "" || len(serverID) > 254-len(egressIPTag) {
		return fmt.Errorf("cannot release neutron port %s. An invalid serverID was provided '%s'", port.ID, serverID)
	}

	if port.DeviceOwner != egressIPTag || port.DeviceID != generateDeviceID(serverID) {
		return fmt.Errorf("cannot delete port '%s' for node with serverID '%s', it belongs to another device owner (%s) and/or device (%s)",
			port.ID, serverID, port.DeviceOwner, port.DeviceID)
	}

	return neutronports.Delete(o.neutronClient, port.ID).ExtractErr()
}

// getNeutronPortWithIPAddressAndMachineID gets the neutron port with the given IP on the given subnet and
// with the correct DeviceID containing the serverID.
func (o *OpenStack) getNeutronPortWithIPAddressAndMachineID(s neutronsubnets.Subnet, ip net.IP, serverID string) (*neutronports.Port, error) {
	if serverID == "" || len(serverID) > 254-len(egressIPTag) {
		return nil, fmt.Errorf("cannot retrieve neutron port with IP address %s on subnet %s with an invalid serverID '%s'", ip.String(), s.ID, serverID)
	}

	var ports []neutronports.Port

	// Loop through all ports on network NetworkID.
	// The following filter does not work, therefore move this logic to the loop below.
	/* FixedIPs: []neutronports.FixedIPOpts{
		{
			SubnetID:  s.ID,
			IPAddress: ip.String(),
		},
	}, */
	// For each port on the network, loop through the ports FixedIPs list and check if
	// SubnetID and IPAddress match with what we're looking for.
	// If so, stop searching the list of ports.
	portListOpts := neutronports.ListOpts{
		NetworkID: s.NetworkID,
	}
	pager := neutronports.List(o.neutronClient, portListOpts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		portList, err := neutronports.ExtractPorts(page)
		if err != nil {
			// Something is wrong, stop searching and throw an error.
			return false, err
		}

		for _, p := range portList {
			if p.DeviceOwner != egressIPTag || p.DeviceID != generateDeviceID(serverID) {
				continue
			}
			for _, fip := range p.FixedIPs {
				if fip.SubnetID == s.ID && fip.IPAddress == ip.String() {
					ports = append(ports, p)
					// End the search.
					return false, nil
				}
			}
		}
		// Get the next list of ports from the pager.
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	if len(ports) != 1 {
		return nil, fmt.Errorf("expected to find a single port, instead found %d ports", len(ports))
	}
	return &ports[0], nil
}

// allowIPAddressOnNeutronPort adds the specified IP address to the port's allowed_address_pairs.
func (o *OpenStack) allowIPAddressOnNeutronPort(portID string, ip net.IP) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Always get the most recent copy of this port.
		p, err := neutronports.Get(o.neutronClient, portID).Extract()
		if err != nil {
			return err
		}

		// Sanity check to see if the IP is already inside the port's allowed_address_pairs.
		if isIPAddressAllowedOnNeutronPort(*p, ip) {
			return AlreadyExistingIPError
		}

		// Update the port's allowed_address_pairs by appending to it.
		// According to the neutron API:
		// "While the ip_address is required, the mac_address will be taken from the port if not specified."
		// https://docs.openstack.org/api-ref/network/v2/index.html?expanded=update-port-detail
		allowedPairs := append(p.AllowedAddressPairs, neutronports.AddressPair{
			IPAddress: ip.String(),
		})
		// Update the port. Provide the revision number to make use of neutron's If-Match
		// header. If the port has received another update since we last retrieved it, the
		// revision number won't match and neutron will return a "RevisionNumberConstraintFailed"
		// error message.
		opts := neutronports.UpdateOpts{
			AllowedAddressPairs: &allowedPairs,
			RevisionNumber:      &p.RevisionNumber,
		}
		_, err = neutronports.Update(o.neutronClient, p.ID, opts).Extract()

		// If the update yielded an error of type "RevisionNumberConstraintFailed", then create a
		// Conflict error. RetryOnConflict will react to this and will repeat the entire operation.
		if err != nil && strings.Contains(err.Error(), "RevisionNumberConstraintFailed") {
			return &apierrors.StatusError{
				ErrStatus: metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonConflict,
					Code:    http.StatusConflict,
				},
			}
		}

		// Any other error or nil, return.
		return err
	})
}

// unallowIPAddressOnNeutronPort removes the specified IP address from the port's allowed_address_pairs.
func (o *OpenStack) unallowIPAddressOnNeutronPort(portID string, ip net.IP) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Always get the most recent copy of this port.
		p, err := neutronports.Get(o.neutronClient, portID).Extract()
		if err != nil {
			return err
		}

		// Sanity check to see if the IP was already removed from the port's allowed_address_pairs.
		// If it's still present, return an error that higher layers should act upon.
		if !isIPAddressAllowedOnNeutronPort(*p, ip) {
			return fmt.Errorf("IP address '%s' is not allowed on port '%s', cannot unallow it", ip, p.ID)
		}

		// Build a slice that contains all allowed pairs other than
		// the one that we want to remove.
		var allowedPairs []neutronports.AddressPair
		for _, aap := range p.AllowedAddressPairs {
			if ip.Equal(net.ParseIP(aap.IPAddress)) {
				continue
			}
			allowedPairs = append(allowedPairs, aap)
		}
		// Update the port. Provide the revision number to make use of neutron's If-Match
		// header. If the port has received another update since we last retrieved it, the
		// revision number won't match and neutron will return a "RevisionNumberConstraintFailed"
		// error message.
		opts := neutronports.UpdateOpts{
			AllowedAddressPairs: &allowedPairs,
			RevisionNumber:      &p.RevisionNumber,
		}
		_, err = neutronports.Update(o.neutronClient, p.ID, opts).Extract()

		// If the update yielded an error of type "RevisionNumberConstraintFailed", then create a
		// Conflict error. RetryOnConflict will react to this and will repeat the entire operation.
		if err != nil && strings.Contains(err.Error(), "RevisionNumberConstraintFailed") {
			return &apierrors.StatusError{
				ErrStatus: metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonConflict,
					Code:    http.StatusConflict,
				},
			}
		}

		// Any other error or nil, return.
		return err
	})
}

// getNeutronSubnetsForNetwork returns all subnets that belong to the given network with
// ID <networkID>.
func (o *OpenStack) getNeutronSubnetsForNetwork(networkID string) ([]neutronsubnets.Subnet, error) {
	var subnets []neutronsubnets.Subnet

	if _, err := uuid.Parse(networkID); err != nil {
		return nil, fmt.Errorf("networkID '%s' is not a valid UUID", networkID)
	}

	opts := neutronsubnets.ListOpts{NetworkID: networkID}
	pager := neutronsubnets.List(o.neutronClient, opts)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		subnetList, err := neutronsubnets.ExtractSubnets(page)
		if err != nil {
			return false, err
		}
		subnets = append(subnets, subnetList...)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return subnets, nil
}

// getNovaServer gets the nova server with ID == <serverID>.
func (o *OpenStack) getNovaServer(serverID string) (*novaservers.Server, error) {
	if _, err := uuid.Parse(serverID); err != nil {
		return nil, fmt.Errorf("serverID '%s' is not a valid UUID", serverID)
	}

	server, err := novaservers.Get(o.novaClient, serverID).Extract()
	if err != nil {
		return nil, err
	}
	return server, nil
}

// listNovaServerPorts lists all ports that are attached to the provided nova server
// with ID == <serverID>.
func (o *OpenStack) listNovaServerPorts(serverID string) ([]neutronports.Port, error) {
	var err error
	var serverPorts []neutronports.Port

	if _, err := uuid.Parse(serverID); err != nil {
		return nil, fmt.Errorf("serverID '%s' is not a valid UUID", serverID)
	}

	portListOpts := neutronports.ListOpts{
		DeviceOwner: novaDeviceOwner,
		DeviceID:    serverID,
	}

	pager := neutronports.List(o.neutronClient, portListOpts)
	err = pager.EachPage(func(page pagination.Page) (bool, error) {
		portList, err := neutronports.ExtractPorts(page)
		if err != nil {
			return false, err
		}
		serverPorts = append(serverPorts, portList...)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return serverPorts, nil
}

// isIPAddressAllowedOnNeutronPort returns true if the given IP address can be found inside the
// list of allowed_address_pairs for this port.
func isIPAddressAllowedOnNeutronPort(p neutronports.Port, ip net.IP) bool {
	for _, aap := range p.AllowedAddressPairs {
		if ip.Equal(net.ParseIP(aap.IPAddress)) {
			return true
		}

	}
	return false
}

// getNovaServerIDFromProviderID extracts the nova server ID from the given providerID.
func getNovaServerIDFromProviderID(providerID string) (string, error) {
	serverID := strings.TrimPrefix(providerID, openstackProviderPrefix)
	if _, err := uuid.Parse(serverID); err != nil {
		return "", fmt.Errorf("cannot parse valid nova server ID from providerId '%s'", providerID)
	}
	return serverID, nil
}

// generateDeviceID is a tiny helper to allow us to work around https://bugzilla.redhat.com/show_bug.cgi?id=2109162.
func generateDeviceID(serverID string) string {
	return fmt.Sprintf("%s_%s", egressIPTag, serverID)
}
