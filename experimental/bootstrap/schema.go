package bootstrap

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/docker/infrakit.aws/plugin/instance"
	"github.com/docker/infrakit/spi/group"
	"strings"
)

const (
	workerType  = "worker"
	managerType = "manager"
	clusterTag  = "infrakit.cluster"
)

type clusterID struct {
	region string
	name   string
}

func (c clusterID) getAWSClient() client.ConfigProvider {
	providers := []credentials.Provider{
		&ec2rolecreds.EC2RoleProvider{Client: ec2metadata.New(session.New())},
		&credentials.EnvProvider{},
		&credentials.SharedCredentialsProvider{},
	}

	return session.New(aws.NewConfig().
		WithRegion(c.region).
		WithCredentialsChainVerboseErrors(true).
		WithCredentials(credentials.NewChainCredentials(providers)).
		WithLogger(&logger{}))
}

func (c clusterID) resourceFilter(vpcID string) []*ec2.Filter {
	return []*ec2.Filter{
		{
			Name:   aws.String("vpc-id"),
			Values: []*string{aws.String(vpcID)},
		},
		c.clusterFilter(),
	}
}

func (c clusterID) clusterFilter() *ec2.Filter {
	return &ec2.Filter{
		Name:   aws.String("tag:" + clusterTag),
		Values: []*string{aws.String(c.name)},
	}
}

func (c clusterID) roleName() string {
	return fmt.Sprintf("%s-ManagerRole", c.name)
}

func (c clusterID) managerPolicyName() string {
	return fmt.Sprintf("%s-ManagerPolicy", c.name)
}

func (c clusterID) instanceProfileName() string {
	return fmt.Sprintf("%s-ManagerProfile", c.name)
}

func (c clusterID) clusterTagMap() map[string]string {
	return map[string]string{clusterTag: c.name}
}

func (c clusterID) resourceTag() *ec2.Tag {
	return &ec2.Tag{
		Key:   aws.String(clusterTag),
		Value: aws.String(c.name),
	}
}

type instanceGroupSpec struct {
	Name   group.ID
	Type   string
	Size   int
	Config instance.CreateInstanceRequest
}

func (i instanceGroupSpec) isManager() bool {
	return i.Type == managerType
}

type clusterSpec struct {
	ClusterName string
	ManagerIPs  []string
	Groups      []instanceGroupSpec
}

func (s *clusterSpec) cluster() clusterID {
	az := s.availabilityZone()
	return clusterID{region: az[:len(az)-1], name: s.ClusterName}
}

func (s *clusterSpec) managers() instanceGroupSpec {
	for _, group := range s.Groups {
		if group.isManager() {
			return group
		}
	}
	panic("No manager group found")
}

func (s *clusterSpec) mutateManagers(op func(*instanceGroupSpec)) {
	s.mutateGroups(func(group *instanceGroupSpec) {
		if group.isManager() {
			op(group)
		}
	})
}

func (s *clusterSpec) mutateGroups(op func(*instanceGroupSpec)) {
	for i, group := range s.Groups {
		op(&group)
		s.Groups[i] = group
	}
}

func applyInstanceDefaults(r *ec2.RunInstancesInput) {
	if r.InstanceType == nil {
		r.InstanceType = aws.String("t2.micro")
	}

	if r.NetworkInterfaces == nil || len(r.NetworkInterfaces) == 0 {
		r.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeleteOnTermination:      aws.Bool(true),
				DeviceIndex:              aws.Int64(0),
			},
		}
	}
}

func (s *clusterSpec) applyDefaults() {
	s.mutateGroups(func(group *instanceGroupSpec) {
		if group.Type == managerType {
			bootLeaderLastOctet := 4
			s.ManagerIPs = []string{}
			for i := 0; i < group.Size; i++ {
				s.ManagerIPs = append(s.ManagerIPs, fmt.Sprintf("192.168.33.%d", bootLeaderLastOctet+i))
			}
		}

		applyInstanceDefaults(&group.Config.RunInstancesInput)
	})
}

func (s *clusterSpec) validate() error {
	errs := []string{}

	addError := func(format string, a ...interface{}) {
		errs = append(errs, fmt.Sprintf(format, a...))
	}

	managerGroups := 0
	workerGroups := 0
	for _, group := range s.Groups {
		switch group.Type {
		case managerType:
			managerGroups++
		case workerType:
			workerGroups++
		default:
			errs = append(
				errs,
				fmt.Sprintf(
					"Invalid instance type '%s', must be %s or %s",
					group.Type,
					workerType,
					managerType))
		}
	}

	if managerGroups != 1 {
		addError("Must specify exactly one group of type %s", managerType)
	}

	/*
		if workerGroups == 0 {
			addError("Must specify exactly one group of type %s", workerType)
		}
	*/

	if s.ClusterName == "" {
		addError("Must specify ClusterName")
	}

	for _, group := range s.Groups {
		if group.isManager() {
			if group.Size != 1 && group.Size != 3 && group.Size != 5 {
				addError("Group %s Size must be 1, 3, or 5", group.Name)
			}
		} else {
			if group.Size < 1 {
				addError("Group %s Size must be at least 1", group.Name)
			}
		}
	}

	validateGroup := func(gid group.ID, group instanceGroupSpec) {
		errorPrefix := fmt.Sprintf("In group %s: ", gid)

		if group.Config.RunInstancesInput.Placement == nil {
			addError(errorPrefix + "run_instance_input.Placement must be set")
		} else if group.Config.RunInstancesInput.Placement.AvailabilityZone == nil ||
			*group.Config.RunInstancesInput.Placement.AvailabilityZone == "" {

			addError(errorPrefix + "run_instance_nput.Placement.AvailabilityZone must be set")
		}
	}

	// MVP restriction - all groups must be in the same Availability Zone.
	firstAz := ""
	for _, group := range s.Groups {
		validateGroup(group.Name, group)

		if group.Config.RunInstancesInput.Placement != nil {
			az := *group.Config.RunInstancesInput.Placement.AvailabilityZone
			if firstAz == "" {
				firstAz = az
			} else if az != firstAz {
				addError(
					"All groups must specify the same run_instance_nput.Placement.AvailabilityZone")
				break
			}
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}

	return nil
}

func (s *clusterSpec) availabilityZone() string {
	for _, group := range s.Groups {
		return *group.Config.RunInstancesInput.Placement.AvailabilityZone
	}
	panic("No groups")
}
