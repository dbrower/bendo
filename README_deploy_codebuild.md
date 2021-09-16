# Codebuild deployment and RPM builds

## History

The Enterprise Systems Unit helped to set up an AWS CodeBuild deployment for the bendo RPM build service which uses the Bendo Docker container maintained by the Curate/Bendo support team to create a new RPM for bendo. The RPM build itself is done using Go (see buildspec.yml) and the CodeBuild project was then added by ESU. 

## CodeBuild deployment

The creation of the CodeBuild project must be performed by someone with credentials/roles which allow for deployment into the LibND VPC. So far, this has been limited to ESU. 

In September, 2021, ansible code was added to perform the deployment. Instead of relying on command line deployment, this (hopefully) makes the deployment a bit clearer.  

Steps to build:
 + Review and update the vars/bendo-codebuild-stack.yml with information about the new deployment
 + Obtain credentials - local credentials needed for deployment
 + Run ansible code:
   +   __ansible-playbook ap-bendo.yml__
   + Ansible will prompt for the following information:
      +  __CodeBuildRole__ : Default is the IAM ESUAdmin role
      +  __LogRetention__  : Default is 400 days
      +  __TargetBucket__  : Default is the bendo-rpm bucket

A new or updated CloudFormation stack will be created when the ansible code is run.

## CodeBuild trigger

A webhook is created by the CloudFormation stack. Once created, updates to the bendo repository will trigger the CodeBuild project to run. Result should be a new RPM based upon tags in the bendo repository.
