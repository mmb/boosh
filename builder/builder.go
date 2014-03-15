package builder

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vito/cloudformer"
)

// amzn-ami-vpc-nat-pv-2013.09.0.x86_64-ebs
var NAT_AMIS = map[string]string{
	"us-east-1":      "ami-ad227cc4",
	"us-west-1":      "ami-d69aad93",
	"us-west-2":      "ami-f032acc0",
	"eu-west-1":      "ami-f3e30084",
	"ap-southeast-1": "ami-f22772a0",
	"ap-southeast-2": "ami-3bae3201",
	"ap-northeast-1": "ami-cd43d9cc",
	"sa-east-1":      "ami-d78325ca",
}

type Builder struct {
	Region string
	Spec   DeploymentSpec
}

func (builder Builder) Build(former cloudformer.CloudFormer) error {
	instances := make(map[string]cloudformer.Instance)
	gateways := make(map[string]cloudformer.InternetGateway)
	subnets := make(map[string]cloudformer.Subnet)
	securityGroups := make(map[string]cloudformer.SecurityGroup)

	vpc := former.VPC("")
	vpc.Network(cloudformer.CIDR(builder.Spec.VPC.CIDR))

	vpc.AssociateDHCPOptions(cloudformer.DHCPOptions{
		DomainNameServers: builder.Spec.DNS,
	})

	for _, x := range builder.Spec.InternetGateways {
		gateways[x.Name] = former.InternetGateway(x.Name)
	}

	vpcGateway, found := gateways[builder.Spec.VPC.InternetGateway]
	if !found {
		return fmt.Errorf("unknown gateway for VPC: %s", builder.Spec.VPC.InternetGateway)
	}

	vpc.AttachInternetGateway(vpcGateway)

	for _, x := range builder.Spec.SecurityGroups {
		group := vpc.SecurityGroup(x.Name)

		for _, i := range x.Ingress {
			fromPort, toPort, err := parsePortRange(i.Ports)
			if err != nil {
				return err
			}

			group.Ingress(
				cloudformer.ProtocolType(i.Protocol),
				cloudformer.CIDR(i.CIDR),
				fromPort,
				toPort,
			)
		}

		for _, e := range x.Egress {
			fromPort, toPort, err := parsePortRange(e.Ports)
			if err != nil {
				return err
			}

			group.Egress(
				cloudformer.ProtocolType(e.Protocol),
				cloudformer.CIDR(e.CIDR),
				fromPort,
				toPort,
			)
		}

		securityGroups[x.Name] = group
	}

	natAMI, found := NAT_AMIS[builder.Region]
	if !found {
		return fmt.Errorf("unknown NAT image for region: %s", builder.Region)
	}

	for _, x := range builder.Spec.Subnets {
		if x.NAT == nil {
			continue
		}

		if x.RouteTable != nil && x.RouteTable.Instance != nil {
			continue
		}

		subnet := vpc.Subnet(x.Name)
		subnet.Network(cloudformer.CIDR(x.CIDR))
		subnet.AvailabilityZone(x.AvailabilityZone)

		if x.RouteTable != nil {
			if x.RouteTable.InternetGateway != nil {
				gateway, found := gateways[*x.RouteTable.InternetGateway]
				if !found {
					return fmt.Errorf("unknown gateway: %s", *x.RouteTable.InternetGateway)
				}

				subnet.RouteTable().InternetGateway(gateway)
			}
		}

		nat := subnet.Instance(x.NAT.Name)
		nat.Type(x.NAT.InstanceType)
		nat.PrivateIP(cloudformer.IP(x.NAT.IP))
		nat.KeyPair(x.NAT.KeyPairName)
		nat.Image(natAMI)
		nat.SourceDestCheck(false)

		securityGroup, found := securityGroups[x.NAT.SecurityGroup]
		if !found {
			return fmt.Errorf("unknown security group: %s", x.NAT.SecurityGroup)
		}

		nat.SecurityGroup(securityGroup)

		ip := former.ElasticIP("NAT")
		ip.Domain("vpc")
		ip.AttachTo(nat)

		instances[x.NAT.Name] = nat
		subnets[x.Name] = subnet
	}

	for _, x := range builder.Spec.Subnets {
		if x.NAT != nil {
			continue
		}

		subnet := vpc.Subnet(x.Name)
		subnet.Network(cloudformer.CIDR(x.CIDR))
		subnet.AvailabilityZone(x.AvailabilityZone)

		if x.RouteTable != nil {
			if x.RouteTable.Instance != nil {
				instance, found := instances[*x.RouteTable.Instance]
				if !found {
					return fmt.Errorf("unknown instance: %s", *x.RouteTable.Instance)
				}

				subnet.RouteTable().Instance(instance)
			}
		}

		subnets[x.Name] = subnet
	}

	for _, x := range builder.Spec.LoadBalancers {
		balancer := former.LoadBalancer(x.Name)

		for _, name := range x.Subnets {
			subnet, found := subnets[name]
			if !found {
				return fmt.Errorf("unknown subnet: %s", name)
			}

			balancer.Subnet(subnet)
		}

		for _, listener := range x.Listeners {
			destinationPort := listener.Port
			if listener.DestinationPort != nil {
				destinationPort = *listener.DestinationPort
			}

			destinationProtocol := listener.Protocol
			if listener.DestinationProtocol != nil {
				destinationProtocol = *listener.DestinationProtocol
			}

			balancer.Listener(
				cloudformer.ProtocolType(listener.Protocol),
				listener.Port,
				cloudformer.ProtocolType(destinationProtocol),
				destinationPort,
			)
		}

		for _, name := range x.SecurityGroups {
			securityGroup, found := securityGroups[name]
			if !found {
				return fmt.Errorf("unknown security group: %s", name)
			}

			balancer.SecurityGroup(securityGroup)
		}

		balancer.HealthCheck(cloudformer.HealthCheck{
			Protocol:           cloudformer.ProtocolType(x.HealthCheck.Target.Type),
			Port:               x.HealthCheck.Target.Port,
			Interval:           time.Duration(x.HealthCheck.Interval) * time.Second,
			Timeout:            time.Duration(x.HealthCheck.Timeout) * time.Second,
			HealthyThreshold:   x.HealthCheck.HealthyThreshold,
			UnhealthyThreshold: x.HealthCheck.UnhealthyThreshold,
		})

		if x.DNSRecord != "" {
			balancer.RecordSet(x.DNSRecord, builder.Spec.Domain)
		}
	}

	for _, x := range builder.Spec.ElasticIPs {
		former.ElasticIP(x.Name).Domain("vpc")
	}

	return nil
}

func parsePortRange(ports string) (uint16, uint16, error) {
	segments := strings.Split(ports, "-")

	fromPortStr := ""
	toPortStr := ""

	if len(segments) == 1 {
		fromPortStr = segments[0]
		toPortStr = fromPortStr
	} else if len(segments) == 2 {
		fromPortStr = segments[0]
		toPortStr = segments[1]
	}

	fromPort, err := strconv.Atoi(strings.Trim(fromPortStr, " "))
	if err != nil {
		return 0, 0, err
	}

	toPort, err := strconv.Atoi(strings.Trim(toPortStr, " "))
	if err != nil {
		return 0, 0, err
	}

	return uint16(fromPort), uint16(toPort), nil
}