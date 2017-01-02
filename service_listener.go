package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"

	"golang.org/x/net/publicsuffix"

	"k8s.io/kubernetes/pkg/client/transport"

	"k8s.io/client-go/kubernetes"
	api "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

// Don't actually commit the changes to route53 records, just print out what we would have done.
var dryRun bool

var currentRecords map[string]string

func init() {
	dryRunStr := os.Getenv("DRY_RUN")
	if dryRunStr != "" {
		dryRun = true
	}
}

func main() {
	flag.Parse()
	glog.Info("Route53 Update Service")

	config, err := rest.InClusterConfig()
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

		config = &rest.Config{
			Host:      apiServer,
			Transport: tlsTransport,
		}
	}

	c, err := kubernetes.NewForConfig(config)
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
	listOptions := api.ListOptions{
		LabelSelector: selector,
	}

	glog.Infof("Starting Service Polling every 30s")
	// TODO(ouziel): delete record if necessary
	currentRecords = make(map[string]string)
	for {
		services, err := c.Services(api.NamespaceAll).List(listOptions)
		if err != nil {
			glog.Fatalf("Failed to list pods: %v", err)
		}

		glog.Infof("Found %d DNS services in all namespaces with selector %q", len(services.Items), selector)
		for i := range services.Items {
			s := &services.Items[i]

			recordValue, recordType, err := serviceRecord(s)
			if err != nil || recordValue == "" {
				glog.Warningf("Couldn't find hostname or IP for %s: %s", s.Name, err)
				continue
			}

			annotation, ok := s.ObjectMeta.Annotations["domainName"]
			if !ok {
				glog.Warningf("Domain name not set for %s", s.Name)
				continue
			}

			domains := strings.Split(annotation, ",")
			for j := range domains {
				recordName := domains[j]

				currentValue, _ := currentRecords[recordName]
				if currentValue == recordValue {
					glog.Infof("DNS already exists for %s service: %s -> %s", s.Name, recordValue, recordName)
					continue
				}

				glog.Infof("Creating DNS for %s service: %s -> %s", s.Name, recordValue, recordName)

				zoneID, err := getDestinationZoneID(r53Api, recordName)
				if err != nil {
					glog.Warningf("Couldn't find destination zone for %s: %s", recordName, err)
					continue
				}

				if err = updateDNS(r53Api, recordValue, recordType, recordName, zoneID); err != nil {
					glog.Warning(err)
					continue
				}
				currentRecords[recordName] = recordValue
				glog.Infof("Created dns record set: domain=%s, zoneID=%s", recordName, zoneID)
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func getDestinationZoneID(r53Api *route53.Route53, domain string) (string, error) {
	zone, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return "", err
	}
	// Since Route53 returns it with dot at the end when listing zones.
	zone += "."

	params := &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(zone),
	}

	resp, err := r53Api.ListHostedZonesByName(params)
	if err != nil {
		return "", err
	}

	// Does binary search on lexicographically ordered hosted zones slice, in
	// order to find the correspondent Route53 zone ID for the given zone name.
	l := len(resp.HostedZones)
	i := sort.Search(l, func(i int) bool {
		return *resp.HostedZones[i].Name == zone
	})

	var zoneID string
	if i < l && *resp.HostedZones[i].Name == zone {
		zoneID = strings.Split(*resp.HostedZones[i].Id, "/")[2]
	} else {
		return "", fmt.Errorf("unable to find hosted zone %q in Route53", zone)
	}

	return zoneID, nil
}

func serviceRecord(service *api.Service) (string, string, error) {
	ingress := service.Status.LoadBalancer.Ingress
	if len(ingress) < 1 {
		return "", "", errors.New("No ingress defined for ELB")
	}
	if len(ingress) > 1 {
		return "", "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	if ingress[0].IP != "" {
		return ingress[0].IP, "A", nil
	}
	if ingress[0].Hostname != "" {
		return ingress[0].Hostname, "CNAME", nil
	}
	return "", "", nil
}

func updateDNS(r53Api *route53.Route53, recordValue, recordType, recordName, zoneID string) error {
	params := &route53.ChangeResourceRecordSetsInput{
	    ChangeBatch: &route53.ChangeBatch{ // Required
	        Changes: []*route53.Change{ // Required
	            { // Required
	                Action: aws.String("UPSERT"), // Required
	                ResourceRecordSet: &route53.ResourceRecordSet{ // Required
	                    Name: aws.String(recordName), // Required
	                    Type: aws.String(recordType),  // Required
						TTL:  aws.Int64(3600),
	                    ResourceRecords: []*route53.ResourceRecord{
	                        { // Required
	                            Value: aws.String(recordValue), // Required
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
