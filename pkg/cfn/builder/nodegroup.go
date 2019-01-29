package builder

import (
	"fmt"

	"github.com/kris-nova/logger"

	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	gfn "github.com/awslabs/goformation/cloudformation"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha4"
	"github.com/weaveworks/eksctl/pkg/cfn/outputs"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
)

// NodeGroupResourceSet stores the resource information of the node group
type NodeGroupResourceSet struct {
	rs               *resourceSet
	clusterSpec      *api.ClusterConfig
	spec             *api.NodeGroup
	provider         api.ClusterProvider
	clusterStackName string
	nodeGroupName    string
	instanceProfile  *gfn.Value
	securityGroups   []*gfn.Value
	vpc              *gfn.Value
	userData         *gfn.Value
}

// NewNodeGroupResourceSet returns a resource set for a node group embedded in a cluster config
func NewNodeGroupResourceSet(provider api.ClusterProvider, spec *api.ClusterConfig, clusterStackName string, ng *api.NodeGroup) *NodeGroupResourceSet {
	return &NodeGroupResourceSet{
		rs:               newResourceSet(),
		clusterStackName: clusterStackName,
		nodeGroupName:    ng.Name,
		clusterSpec:      spec,
		spec:             ng,
		provider:         provider,
	}
}

// AddAllResources adds all the information about the node group to the resource set
func (n *NodeGroupResourceSet) AddAllResources() error {
	n.rs.template.Description = fmt.Sprintf(
		"%s (AMI family: %s, SSH access: %v, subnet topology: %s) %s",
		nodeGroupTemplateDescription,
		n.spec.AMIFamily, n.spec.AllowSSH, n.spec.SubnetTopology(),
		templateDescriptionSuffix)

	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeaturePrivateNetworking, n.spec.PrivateNetworking, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureSharedSecurityGroup, n.spec.SecurityGroups.WithShared, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureLocalSecurityGroup, n.spec.SecurityGroups.WithLocal, false)

	n.vpc = makeImportValue(n.clusterStackName, outputs.ClusterVPC)

	userData, err := nodebootstrap.NewUserData(n.clusterSpec, n.spec)
	if err != nil {
		return err
	}
	n.userData = gfn.NewString(userData)

	switch {
	case n.spec.MinSize == 0 && n.spec.MaxSize == 0:
		n.spec.MinSize = n.spec.DesiredCapacity
		n.spec.MaxSize = n.spec.DesiredCapacity
	case n.spec.MinSize > 0 && n.spec.MaxSize > 0:
		if n.spec.DesiredCapacity == api.DefaultNodeCount {
			msgPrefix := fmt.Sprintf("as --nodes-min=%d and --nodes-max=%d were given", n.spec.MinSize, n.spec.MaxSize)
			if n.spec.DesiredCapacity < n.spec.MinSize {
				n.spec.DesiredCapacity = n.spec.MaxSize
				logger.Info("%s, --nodes=%d was set automatically as default value (--node=%d) was outside the set renge",
					msgPrefix, n.spec.DesiredCapacity, api.DefaultNodeCount)
			} else {
				logger.Info("%s, default value of --nodes=%d was kept as it is within the set range",
					msgPrefix, n.spec.DesiredCapacity)
			}
		}
		if n.spec.DesiredCapacity > n.spec.MaxSize {
			return fmt.Errorf("cannot use --nodes-max=%d and --nodes=%d at the same time", n.spec.MaxSize, n.spec.DesiredCapacity)
		}
	}

	n.addResourcesForIAM()
	n.addResourcesForSecurityGroups()

	return n.addResourcesForNodeGroup()
}

// RenderJSON returns the rendered JSON
func (n *NodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return n.rs.renderJSON()
}

// Template returns the CloudFormation template
func (n *NodeGroupResourceSet) Template() gfn.Template {
	return *n.rs.template
}

func (n *NodeGroupResourceSet) newResource(name string, resource interface{}) *gfn.Value {
	return n.rs.newResource(name, resource)
}

func (n *NodeGroupResourceSet) addResourcesForNodeGroup() error {
	lc := &gfn.AWSAutoScalingLaunchConfiguration{
		IamInstanceProfile: n.instanceProfile,
		SecurityGroups:     n.securityGroups,
		ImageId:            gfn.NewString(n.spec.AMI),
		InstanceType:       gfn.NewString(n.spec.InstanceType),
		UserData:           n.userData,
	}
	if n.spec.AllowSSH {
		lc.KeyName = gfn.NewString(n.spec.SSHPublicKeyName)
	}
	if n.spec.PrivateNetworking {
		lc.AssociatePublicIpAddress = gfn.False()
	} else {
		lc.AssociatePublicIpAddress = gfn.True()
	}
	if n.spec.VolumeSize > 0 {
		lc.BlockDeviceMappings = []gfn.AWSAutoScalingLaunchConfiguration_BlockDeviceMapping{
			{
				DeviceName: gfn.NewString("/dev/xvda"),
				Ebs: &gfn.AWSAutoScalingLaunchConfiguration_BlockDevice{
					VolumeSize: gfn.NewInteger(n.spec.VolumeSize),
					VolumeType: gfn.NewString(n.spec.VolumeType),
				},
			},
		}
	}
	refLC := n.newResource("NodeLaunchConfig", lc)
	// currently goformation type system doesn't allow specifying `VPCZoneIdentifier: { "Fn::ImportValue": ... }`,
	// and tags don't have `PropagateAtLaunch` field, so we have a custom method here until this gets resolved
	var vpcZoneIdentifier interface{}
	if numNodeGroupsAZs := len(n.spec.AvailabilityZones); numNodeGroupsAZs > 0 {
		subnets := n.clusterSpec.VPC.Subnets[n.spec.SubnetTopology()]
		errorDesc := fmt.Sprintf("(subnets=%#v AZs=%#v)", subnets, n.spec.AvailabilityZones)
		if len(subnets) < numNodeGroupsAZs {
			return fmt.Errorf("VPC doesn't have enough subnets for nodegroup AZs %s", errorDesc)
		}
		vpcZoneIdentifier = make([]interface{}, numNodeGroupsAZs)
		for i, az := range n.spec.AvailabilityZones {
			subnet, ok := subnets[az]
			if !ok {
				return fmt.Errorf("VPC doesn't have subnets in %s %s", az, errorDesc)
			}
			vpcZoneIdentifier.([]interface{})[i] = subnet.ID
		}
	} else {
		vpcZoneIdentifier = map[string][]interface{}{
			gfn.FnSplit: []interface{}{
				",",
				makeImportValue(n.clusterStackName, outputs.ClusterSubnets+string(n.spec.SubnetTopology())),
			},
		}
	}
	tags := []map[string]interface{}{
		{
			"Key":               "Name",
			"Value":             fmt.Sprintf("%s-%s-Node", n.clusterSpec.Metadata.Name, n.nodeGroupName),
			"PropagateAtLaunch": "true",
		},
		{
			"Key":               "kubernetes.io/cluster/" + n.clusterSpec.Metadata.Name,
			"Value":             "owned",
			"PropagateAtLaunch": "true",
		},
	}
	if v := n.spec.IAM.WithAddonPolicies.AutoScaler; v != nil && *v {
		tags = append(tags,
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/enabled",
				"Value":             "true",
				"PropagateAtLaunch": "true",
			},
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/" + n.clusterSpec.Metadata.Name,
				"Value":             "owned",
				"PropagateAtLaunch": "true",
			},
		)
	}
	n.newResource("NodeGroup", &awsCloudFormationResource{
		Type: "AWS::AutoScaling::AutoScalingGroup",
		Properties: map[string]interface{}{
			"LaunchConfigurationName": refLC,
			"DesiredCapacity":         fmt.Sprintf("%d", n.spec.DesiredCapacity),
			"MinSize":                 fmt.Sprintf("%d", n.spec.MinSize),
			"MaxSize":                 fmt.Sprintf("%d", n.spec.MaxSize),
			"VPCZoneIdentifier":       vpcZoneIdentifier,
			"Tags":                    tags,
		},
		UpdatePolicy: map[string]map[string]string{
			"AutoScalingRollingUpdate": {
				"MinInstancesInService": "1",
				"MaxBatchSize":          "1",
			},
		},
	})

	return nil
}

// GetAllOutputs collects all outputs of the node group
func (n *NodeGroupResourceSet) GetAllOutputs(stack cfn.Stack) error {
	return n.rs.GetAllOutputs(stack)
}
