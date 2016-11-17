package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/transport"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
)

// Don't actually commit the changes to route53 records, just print out what we would have done.
var dryRun bool

func init() {
	dryRunStr := os.Getenv("DRY_RUN")
	if dryRunStr != "" {
		dryRun = true
	}
}

func main() {
	flag.Parse()
	glog.Info("Route53 Update Service")

	config, err := restclient.InClusterConfig()
	if err != nil {
		kubernetesService := os.Getenv("KUBERNETES_SERVICE_HOST")
		kubernetesServicePort := os.Getenv("KUBERNETES_SERVICE_PORT")
		if kubernetesService == "" {
			glog.Fatal("Please specify the Kubernetes server via KUBERNETES_SERVICE_HOST")
		}
		if kubernetesServicePort == "" {
			kubernetesServicePort = "443"
		}
		apiServer := fmt.Sprintf("https://%s:%s", kubernetesService, kubernetesServicePort)

		caFilePath := os.Getenv("CA_FILE_PATH")
		certFilePath := os.Getenv("CERT_FILE_PATH")
		keyFilePath := os.Getenv("KEY_FILE_PATH")
		if caFilePath == "" || certFilePath == "" || keyFilePath == "" {
			glog.Fatal("You must provide paths for CA, Cert, and Key files")
		}

		tls := transport.TLSConfig{
			CAFile:   caFilePath,
			CertFile: certFilePath,
			KeyFile:  keyFilePath,
		}
		// tlsTransport := transport.New(transport.Config{TLS: tls})
		tlsTransport, err := transport.New(&transport.Config{TLS: tls})
		if err != nil {
			glog.Fatalf("Couldn't set up tls transport: %s", err)
		}

		config = &restclient.Config{
			Host:      apiServer,
			Transport: tlsTransport,
		}
	}

	c, err := client.New(config)
	if err != nil {
		glog.Fatalf("Failed to make client: %v", err)
	}
	glog.Infof("Connected to kubernetes @ %s", config.Host)

	metadata := ec2metadata.New(session.New())

	creds := credentials.NewChainCredentials(
		[]credentials.Provider{
			&credentials.EnvProvider{},
			&credentials.SharedCredentialsProvider{},
			&ec2rolecreds.EC2RoleProvider{Client: metadata},
		})

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region, err = metadata.Region()
		if err != nil {
			glog.Fatalf("Unable to retrieve the region from environment or the EC2 instance %v\n", err)
		}
	}

	awsConfig := aws.NewConfig()
	awsConfig.WithCredentials(creds)
	awsConfig.WithRegion(region)
	sess := session.New(awsConfig)

	r53Api := route53.New(sess)
	if r53Api == nil {
		glog.Fatal("Failed to make AWS connection")
	}

	selector := "dns=route53"
	l, err := labels.Parse(selector)
	if err != nil {
		glog.Fatalf("Failed to parse selector %q: %v", selector, err)
	}
	listOptions := api.ListOptions{
		LabelSelector: l,
	}

	glog.Infof("Starting Service Polling every 30s")
	for {
		services, err := c.Services(api.NamespaceAll).List(listOptions)
		if err != nil {
			glog.Fatalf("Failed to list pods: %v", err)
		}

		glog.Infof("Found %d DNS services in all namespaces with selector %q", len(services.Items), selector)
		for i := range services.Items {
			s := &services.Items[i]

			hostOrIp, err := serviceHostname(s)
			recordType := "CNAME"
			if err != nil || hostOrIp == "" {
				glog.Warningf("Couldn't find hostname for %s: %s", s.Name, err)
				hostOrIp, err = serviceIP(s)
				if err != nil || hostOrIp == "" {
					glog.Warningf("Couldn't find IP for %s: %s", s.Name, err)
					continue
				} else {
					recordType = "A"
				}
			}

			annotation, ok := s.ObjectMeta.Annotations["domainName"]
			if !ok {
				glog.Warningf("Domain name not set for %s", s.Name)
				continue
			}

			domains := strings.Split(annotation, ",")
			for j := range domains {
				domain := domains[j]

				glog.Infof("Creating DNS for %s service: %s -> %s", s.Name, hostOrIp, domain)

				zone, err := getDestinationZone(domain, r53Api)
				if err != nil {
					glog.Warningf("Couldn't find destination zone: %s", err)
					continue
				}

				zoneID := *zone.Id
				zoneParts := strings.Split(zoneID, "/")
				zoneID = zoneParts[len(zoneParts)-1]

				if err = updateDNS(r53Api, hostOrIp, recordType, strings.TrimLeft(domain, "."), zoneID); err != nil {
					glog.Warning(err)
					continue
				}
				glog.Infof("Created dns record set: domain=%s, zoneID=%s", domain, zoneID)
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func getDestinationZone(domain string, r53Api *route53.Route53) (*route53.HostedZone, error) {
	tld, err := getTLD(domain)
	if err != nil {
		return nil, err
	}

	listHostedZoneInput := route53.ListHostedZonesByNameInput{
		DNSName: &tld,
	}
	hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
	if err != nil {
		return nil, fmt.Errorf("No zone found for %s: %v", tld, err)
	}
	// TODO: The AWS API may return multiple pages, we should parse them all

	return findMostSpecificZoneForDomain(domain, hzOut.HostedZones)
}

func findMostSpecificZoneForDomain(domain string, zones []*route53.HostedZone) (*route53.HostedZone, error) {
	domain = domainWithTrailingDot(domain)
	if len(zones) < 1 {
		return nil, fmt.Errorf("No zone found for %s", domain)
	}
	var mostSpecific *route53.HostedZone
	curLen := 0

	for i := range zones {
		zone := zones[i]
		zoneName := *zone.Name

		if strings.HasSuffix(domain, zoneName) && curLen < len(zoneName) {
			curLen = len(zoneName)
			mostSpecific = zone
		}
	}

	if mostSpecific == nil {
		return nil, fmt.Errorf("Zone found %s does not match domain given %s", *zones[0].Name, domain)
	}

	return mostSpecific, nil
}

func getTLD(domain string) (string, error) {
	domainParts := strings.Split(domain, ".")
	segments := len(domainParts)
	if segments < 3 {
		return "", fmt.Errorf("Domain %s is invalid - it should be a fully qualified domain name and subdomain (i.e. test.example.com)", domain)
	}
	return strings.Join(domainParts[segments-2:], "."), nil
}

func domainWithTrailingDot(withoutDot string) string {
	if withoutDot[len(withoutDot)-1:] == "." {
		return withoutDot
	}
	return fmt.Sprint(withoutDot, ".")
}

func serviceHostname(service *api.Service) (string, error) {
	ingress := service.Status.LoadBalancer.Ingress
	if len(ingress) < 1 {
		return "", errors.New("No ingress defined for ELB")
	}
	if len(ingress) > 1 {
		return "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	return ingress[0].Hostname, nil
}

func serviceIP(service *api.Service) (string, error) {
	ingress := service.Status.LoadBalancer.Ingress
	if len(ingress) < 1 {
		return "", errors.New("No ingress defined for ELB")
	}
	if len(ingress) > 1 {
		return "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	return ingress[0].IP, nil
}

func updateDNS(r53Api *route53.Route53, hostOrIp, recordType, domain, zoneID string) error {
	glog.Infof("hostOrIp: %s", hostOrIp)
	glog.Infof("recordType: %s", recordType)
	glog.Infof("domain: %s", domain)
	glog.Infof("zoneID: %s", zoneID)

	params := &route53.ChangeResourceRecordSetsInput{
	    ChangeBatch: &route53.ChangeBatch{ // Required
	        Changes: []*route53.Change{ // Required
	            { // Required
	                Action: aws.String("UPSERT"), // Required
	                ResourceRecordSet: &route53.ResourceRecordSet{ // Required
	                    Name: aws.String(domain), // Required
	                    Type: aws.String(recordType),  // Required
	                    ResourceRecords: []*route53.ResourceRecord{
	                        { // Required
	                            Value: aws.String(hostOrIp), // Required
	                        },
	                    },
	                },
	            },
	        },
	        Comment: aws.String("Kubernetes Update to Service"),
	    },
	    HostedZoneId: aws.String(zoneID), // Required
	}

	resp, err := r53Api.ChangeResourceRecordSets(params)
	glog.Infof("Response: %v", resp)
	if err != nil {
		return fmt.Errorf("Failed to update record set: %v", err)
	}

	return nil
}
