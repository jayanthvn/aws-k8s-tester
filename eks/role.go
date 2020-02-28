package eks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	awscfn "github.com/aws/aws-k8s-tester/pkg/aws/cloudformation"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"go.uber.org/zap"
)

// TemplateClusterRoleBasic is the CloudFormation template for EKS cluster role.
const TemplateClusterRoleBasic = `
---
AWSTemplateFormatVersion: '2010-09-09'
Description: 'Amazon EKS Cluster Role Basic'

Parameters:

  ClusterRoleName:
    Description: EKS Role name
    Type: String

  ClusterRoleServicePrincipals:
    Description: EKS Role Service Principals
    Type: CommaDelimitedList
    Default: eks.amazonaws.com

  ClusterRoleManagedPolicyARNs:
    Description: EKS Role managed policy ARNs
    Type: CommaDelimitedList
    Default: 'arn:aws:iam::aws:policy/AmazonEKSServicePolicy,arn:aws:iam::aws:policy/AmazonEKSClusterPolicy'

Resources:

  ClusterRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: !Ref ClusterRoleName
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
        - Effect: Allow
          Principal:
            Service: !Ref ClusterRoleServicePrincipals
          Action:
          - sts:AssumeRole
      ManagedPolicyArns: !Ref ClusterRoleManagedPolicyARNs
      Path: /

Outputs:

  ClusterRoleARN:
    Description: Cluster role ARN that EKS uses to create AWS resources for Kubernetes
    Value: !GetAtt ClusterRole.Arn

`

// TemplateClusterRoleNLB is the CloudFormation template for EKS cluster role
// with policies required for NLB service operation.
//
// e.g.
//   Error creating load balancer (will retry): failed to ensure load balancer for service eks-*/hello-world-service: Error creating load balancer: "AccessDenied: User: arn:aws:sts::404174646922:assumed-role/eks-*-cluster-role/* is not authorized to perform: ec2:DescribeAccountAttributes\n\tstatus code: 403"
//
// TODO: scope down (e.g. ec2:DescribeAccountAttributes, ec2:DescribeInternetGateways)
const TemplateClusterRoleNLB = `
---
AWSTemplateFormatVersion: '2010-09-09'
Description: 'Amazon EKS Cluster Role + NLB'

Parameters:

  ClusterRoleName:
    Description: EKS Role name
    Type: String

  ClusterRoleServicePrincipals:
    Description: EKS Role Service Principals
    Type: CommaDelimitedList
    Default: eks.amazonaws.com

  ClusterRoleManagedPolicyARNs:
    Description: EKS Role managed policy ARNs
    Type: CommaDelimitedList
    Default: 'arn:aws:iam::aws:policy/AmazonEKSServicePolicy,arn:aws:iam::aws:policy/AmazonEKSClusterPolicy'

Resources:

  ClusterRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: !Ref ClusterRoleName
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
        - Effect: Allow
          Principal:
            Service: !Ref ClusterRoleServicePrincipals
          Action:
          - sts:AssumeRole
      ManagedPolicyArns: !Ref ClusterRoleManagedPolicyARNs
      Path: /
      Policies:
      - PolicyName: !Join ['-', [!Ref ClusterRoleName, 'nlb-policy']]
        PolicyDocument:
          Version: '2012-10-17'
          Statement:
          - Action:
            - ec2:*
            Effect: Allow
            Resource: '*'

Outputs:

  ClusterRoleARN:
    Description: Cluster role ARN that EKS uses to create AWS resources for Kubernetes
    Value: !GetAtt ClusterRole.Arn

`

func (ts *Tester) createClusterRole() error {
	if !ts.cfg.Parameters.ClusterRoleCreate {
		ts.lg.Info("Parameters.ClusterRoleCreate false; skipping creation")
		return nil
	}
	if ts.cfg.Parameters.ClusterRoleARN != "" ||
		ts.cfg.Status.ClusterRoleCFNStackID != "" ||
		ts.cfg.Status.ClusterRoleARN != "" {
		ts.lg.Info("non-empty role given; no need to create a new one")
		return nil
	}
	if ts.cfg.Parameters.ClusterRoleName == "" {
		return errors.New("empty Parameters.ClusterRoleName")
	}

	tmpl := TemplateClusterRoleBasic
	if ts.cfg.AddOnNLBHelloWorld.Enable {
		tmpl = TemplateClusterRoleNLB
	}

	// role ARN is empty, create a default role
	// otherwise, use the existing one
	ts.lg.Info("creating a new role", zap.String("cluster-role-name", ts.cfg.Parameters.ClusterRoleName))
	stackInput := &cloudformation.CreateStackInput{
		StackName:    aws.String(ts.cfg.Parameters.ClusterRoleName),
		Capabilities: aws.StringSlice([]string{"CAPABILITY_NAMED_IAM"}),
		OnFailure:    aws.String(cloudformation.OnFailureDelete),
		TemplateBody: aws.String(tmpl),
		Tags: awscfn.NewTags(map[string]string{
			"Kind": "aws-k8s-tester",
			"Name": ts.cfg.Name,
		}),
		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("ClusterRoleName"),
				ParameterValue: aws.String(ts.cfg.Parameters.ClusterRoleName),
			},
		},
	}
	if len(ts.cfg.Parameters.ClusterRoleServicePrincipals) > 0 {
		ts.lg.Info("creating a new cluster role with custom service principals",
			zap.Strings("service-principals", ts.cfg.Parameters.ClusterRoleServicePrincipals),
		)
		stackInput.Parameters = append(stackInput.Parameters, &cloudformation.Parameter{
			ParameterKey:   aws.String("ClusterRoleServicePrincipals"),
			ParameterValue: aws.String(strings.Join(ts.cfg.Parameters.ClusterRoleServicePrincipals, ",")),
		})
	}
	if len(ts.cfg.Parameters.ClusterRoleManagedPolicyARNs) > 0 {
		ts.lg.Info("creating a new cluster role with custom managed role policies",
			zap.Strings("policy-arns", ts.cfg.Parameters.ClusterRoleManagedPolicyARNs),
		)
		stackInput.Parameters = append(stackInput.Parameters, &cloudformation.Parameter{
			ParameterKey:   aws.String("ClusterRoleManagedPolicyARNs"),
			ParameterValue: aws.String(strings.Join(ts.cfg.Parameters.ClusterRoleManagedPolicyARNs, ",")),
		})
	}
	stackOutput, err := ts.cfnAPI.CreateStack(stackInput)
	if err != nil {
		return err
	}
	ts.cfg.Status.ClusterRoleCFNStackID = aws.StringValue(stackOutput.StackId)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	ch := awscfn.Poll(
		ctx,
		ts.stopCreationCh,
		ts.interruptSig,
		ts.lg,
		ts.cfnAPI,
		ts.cfg.Status.ClusterRoleCFNStackID,
		cloudformation.ResourceStatusCreateComplete,
		25*time.Second,
		10*time.Second,
	)
	var st awscfn.StackStatus
	for st = range ch {
		if st.Error != nil {
			cancel()
			ts.cfg.RecordStatus(fmt.Sprintf("failed to create role (%v)", st.Error))
			ts.lg.Warn("polling errror", zap.Error(st.Error))
		}
	}
	cancel()
	if st.Error != nil {
		return st.Error
	}
	// update status after creating a new IAM role
	for _, o := range st.Stack.Outputs {
		switch k := aws.StringValue(o.OutputKey); k {
		case "ClusterRoleARN":
			ts.cfg.Status.ClusterRoleARN = aws.StringValue(o.OutputValue)
		default:
			return fmt.Errorf("unexpected OutputKey %q from %q", k, ts.cfg.Status.ClusterRoleCFNStackID)
		}
	}

	ts.lg.Info("created a new role",
		zap.String("cluster-role-cfn-stack-id", ts.cfg.Status.ClusterRoleCFNStackID),
		zap.String("cluster-role-arn", ts.cfg.Status.ClusterRoleARN),
		zap.String("cluster-role-name", ts.cfg.Parameters.ClusterRoleName),
	)
	return ts.cfg.Sync()
}

func (ts *Tester) deleteClusterRole() error {
	if !ts.cfg.Parameters.ClusterRoleCreate {
		ts.lg.Info("Parameters.ClusterRoleCreate false; skipping deletion")
		return nil
	}
	if ts.cfg.Status.ClusterRoleCFNStackID == "" {
		ts.lg.Info("empty role CFN stack ID; no need to delete role")
		return nil
	}

	ts.lg.Info("deleting role CFN stack", zap.String("cluster-role-cfn-stack-id", ts.cfg.Status.ClusterRoleCFNStackID))
	_, err := ts.cfnAPI.DeleteStack(&cloudformation.DeleteStackInput{
		StackName: aws.String(ts.cfg.Status.ClusterRoleCFNStackID),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	ch := awscfn.Poll(
		ctx,
		make(chan struct{}),  // do not exit on stop
		make(chan os.Signal), // do not exit on stop
		ts.lg,
		ts.cfnAPI,
		ts.cfg.Status.ClusterRoleCFNStackID,
		cloudformation.ResourceStatusDeleteComplete,
		25*time.Second,
		10*time.Second,
	)
	var st awscfn.StackStatus
	for st = range ch {
		if st.Error != nil {
			cancel()
			ts.cfg.RecordStatus(fmt.Sprintf("failed to delete role (%v)", st.Error))
			ts.lg.Warn("polling errror", zap.Error(st.Error))
		}
	}
	cancel()
	if st.Error != nil {
		return st.Error
	}
	ts.lg.Info("deleted a role",
		zap.String("cluster-role-cfn-stack-id", ts.cfg.Status.ClusterRoleCFNStackID),
		zap.String("cluster-role-arn", ts.cfg.Status.ClusterRoleARN),
		zap.String("cluster-role-name", ts.cfg.Parameters.ClusterRoleName),
	)
	return ts.cfg.Sync()
}
