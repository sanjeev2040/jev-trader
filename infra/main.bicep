// jev-trader production infrastructure (reports/0003-deployment.md, option B):
// one Linux VM in Central India that is started each NSE trading morning by a
// GitHub Actions cron and deallocated by Azure auto-shutdown at 16:00 IST.
//
// Deploy it yourself from your own `az login`, into an existing resource group
// (see deploy/README.md). CI never gets rights to change infrastructure.
//
// No secret values live here: Key Vault is created empty and you enter the
// secrets by hand.

targetScope = 'resourceGroup'

@description('Azure region. The static IP is registered with Kite, so a region change means a new IP and a new whitelist entry.')
param location string = 'centralindia'

@description('VM size. B2als v2 (2 vCPU, 4 GiB). Move to Standard_B2ats_v2 only after measuring memory over a recorded session.')
param vmSize string = 'Standard_B2als_v2'

@description('VM name. The Release and Market hours workflows read it from the VM_NAME variable.')
param vmName string = 'vm-jevtrader'

@description('Linux admin user.')
param adminUsername string = 'jevadmin'

@description('Public SSH key for the admin user (the contents of your .pub file). Password logins are disabled.')
param adminSshPublicKey string

@description('The only address allowed to reach SSH (port 22), as a CIDR such as 203.0.113.7/32. Never 0.0.0.0/0.')
param operatorCidr string

@description('Container registry name: 5-50 lowercase letters and digits, globally unique.')
@minLength(5)
@maxLength(50)
param acrName string = 'jevtrader${uniqueString(resourceGroup().id)}'

@description('Key Vault name: 3-24 characters, globally unique.')
@minLength(3)
@maxLength(24)
param keyVaultName string = 'kv-jev-${take(uniqueString(resourceGroup().id), 10)}'

@description('Backup storage account name: 3-24 lowercase letters and digits, globally unique.')
@minLength(3)
@maxLength(24)
param storageAccountName string = 'stjev${uniqueString(resourceGroup().id)}'

@description('Blob container for the nightly backup.')
param backupContainerName string = 'backups'

@description('GitHub repository (owner/name) whose workflows may log in through OIDC.')
param githubRepo string = 'sanjeev2040/jev-trader'

@description('Instruments `jevtrader run` trades, comma separated EXCHANGE:SYMBOL keys, for example NSE:INFY,NSE:TCS.')
param watchlist string

@description('Pinned Jev model id passed to `jevtrader run --model`.')
param jevModel string

@description('Email address for the action group and the budget alerts.')
param alertEmail string

@description('Monthly budget in USD. Alerts only; nothing is stopped.')
param budgetAmount int = 25

@description('First day of the month the budget starts in, YYYY-MM-01. It must be the first of a month and cannot be changed later.')
param budgetStartDate string = '2026-10-01'

@description('Optional: your Entra object id. When set you get Key Vault Secrets Officer, so you can enter the secret values.')
param operatorObjectId string = ''

// Built-in role definition ids.
var roles = {
  acrPull: '7f951dda-4ed3-4680-a7ca-43fe172d538d'
  acrPush: '8311e382-0749-4cb8-b61a-304f252e45ec'
  keyVaultSecretsUser: '4633458b-17de-408a-b874-0521c4a2b1c3'
  keyVaultSecretsOfficer: 'b86a8fe4-44ce-4948-aee5-eccb2c155cd7'
  storageBlobDataContributor: 'ba92f5b4-2d11-453d-a403-e96b0029c9fe'
  tagContributor: '4a9ae827-6dc8-4573-8ac7-8239d42aa03f'
}

func roleId(id string) string => subscriptionResourceId('Microsoft.Authorization/roleDefinitions', id)

// ---------------------------------------------------------------- network

resource nsg 'Microsoft.Network/networkSecurityGroups@2024-01-01' = {
  name: 'nsg-jevtrader'
  location: location
  properties: {
    securityRules: [
      {
        name: 'ssh-from-operator'
        properties: {
          priority: 1000
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourceAddressPrefix: operatorCidr
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '22'
        }
      }
    ]
  }
}

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: 'vnet-jevtrader'
  location: location
  properties: {
    addressSpace: { addressPrefixes: ['10.20.0.0/24'] }
    subnets: [
      {
        name: 'default'
        properties: {
          addressPrefix: '10.20.0.0/26'
          networkSecurityGroup: { id: nsg.id }
        }
      }
    ]
  }
}

// The Standard static IP is the VM's outbound address too. It is registered
// with Kite, so it must never be recreated: it carries a CanNotDelete lock.
resource pip 'Microsoft.Network/publicIPAddresses@2024-01-01' = {
  name: 'pip-jevtrader'
  location: location
  sku: { name: 'Standard', tier: 'Regional' }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}

resource pipLock 'Microsoft.Authorization/locks@2020-05-01' = {
  name: 'protect-static-ip'
  scope: pip
  properties: {
    level: 'CanNotDelete'
    notes: 'Registered in the Kite developer console. A new address needs a new whitelist entry. See reports/0003-deployment.md.'
  }
}

resource nic 'Microsoft.Network/networkInterfaces@2024-01-01' = {
  name: 'nic-jevtrader'
  location: location
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          privateIPAllocationMethod: 'Dynamic'
          subnet: { id: '${vnet.id}/subnets/default' }
          publicIPAddress: { id: pip.id }
        }
      }
    ]
  }
}

// ---------------------------------------------------------------- registry, vault, storage

resource acr 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: acrName
  location: location
  sku: { name: 'Basic' }
  properties: {
    adminUserEnabled: false // identities only: no registry password exists
  }
}

// RBAC mode, created empty. Enter kite-api-key, kite-api-secret and
// jev-api-key yourself (deploy/README.md); they never pass through GitHub.
resource kv 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: keyVaultName
  location: location
  properties: {
    tenantId: subscription().tenantId
    sku: { family: 'A', name: 'standard' }
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 30
  }
}

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountName
  location: location
  kind: 'StorageV2'
  sku: { name: 'Standard_LRS' }
  properties: {
    accessTier: 'Hot'
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false // the VM uses its identity; no account key is used anywhere
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storage
  name: 'default'
}

resource backups 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobService
  name: backupContainerName
}

// ---------------------------------------------------------------- VM

resource vm 'Microsoft.Compute/virtualMachines@2024-03-01' = {
  name: vmName
  location: location
  identity: { type: 'SystemAssigned' }
  // jev-image is set by the Release workflow and read by the VM at boot. It is
  // deliberately not declared here, so a redeploy of this template cannot reset it.
  properties: {
    hardwareProfile: { vmSize: vmSize }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: 'ubuntu-24_04-lts'
        sku: 'server'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        diskSizeGB: 64
        managedDisk: { storageAccountType: 'StandardSSD_LRS' }
      }
    }
    osProfile: {
      computerName: vmName
      adminUsername: adminUsername
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/${adminUsername}/.ssh/authorized_keys'
              keyData: adminSshPublicKey
            }
          ]
        }
      }
    }
    networkProfile: { networkInterfaces: [{ id: nic.id }] }
  }
}

// Azure auto-shutdown deallocates the VM (and stops compute billing). A
// guest-OS shutdown would not. The name must be exactly this.
resource autoShutdown 'Microsoft.DevTestLab/schedules@2018-09-15' = {
  name: 'shutdown-computevm-${vm.name}'
  location: location
  properties: {
    status: 'Enabled'
    taskType: 'ComputeVmShutdownTask'
    dailyRecurrence: { time: '1600' }
    timeZoneId: 'India Standard Time'
    targetResourceId: vm.id
    notificationSettings: { status: 'Disabled', timeInMinutes: 30 }
  }
}

// The scripts and units under deploy/vm, installed by the CustomScript
// extension. Changing them changes forceUpdateTag, so a redeploy re-runs the
// (idempotent) install, but only while the VM is running: start it first.
var payload = [
  { name: 'lib.sh', b64: loadFileAsBase64('../deploy/vm/lib.sh') }
  { name: 'prepare.sh', b64: loadFileAsBase64('../deploy/vm/prepare.sh') }
  { name: 'wait-for-login.sh', b64: loadFileAsBase64('../deploy/vm/wait-for-login.sh') }
  { name: 'run.sh', b64: loadFileAsBase64('../deploy/vm/run.sh') }
  { name: 'login.sh', b64: loadFileAsBase64('../deploy/vm/login.sh') }
  { name: 'backup.sh', b64: loadFileAsBase64('../deploy/vm/backup.sh') }
  { name: 'provision.sh', b64: loadFileAsBase64('../deploy/vm/provision.sh') }
  { name: 'jevtrader-prepare.service', b64: loadFileAsBase64('../deploy/vm/jevtrader-prepare.service') }
  { name: 'jevtrader-run.service', b64: loadFileAsBase64('../deploy/vm/jevtrader-run.service') }
  { name: 'jevtrader-backup.service', b64: loadFileAsBase64('../deploy/vm/jevtrader-backup.service') }
  { name: 'jevtrader-backup.timer', b64: loadFileAsBase64('../deploy/vm/jevtrader-backup.timer') }
]
var unpack = join(map(payload, f => 'echo ${f.b64} | base64 -d > "$d/${f.name}"'), '\n')
// Single quotes are dropped so a value cannot break out of its quoting.
var settings = join([
  'export ACR_LOGIN_SERVER=\'${acr.name}.${environment().suffixes.acrLoginServer}\''
  'export KEY_VAULT_NAME=\'${kv.name}\''
  'export BACKUP_ACCOUNT=\'${storage.name}\''
  'export BACKUP_CONTAINER=\'${backupContainerName}\''
  'export WATCHLIST=\'${replace(watchlist, '\'', '')}\''
  'export JEV_MODEL=\'${replace(jevModel, '\'', '')}\''
], '\n')
var provisionScript = join([
  '#!/bin/bash'
  'set -euo pipefail'
  'd=$(mktemp -d)'
  'trap \'rm -rf "$d"\' EXIT'
  unpack
  settings
  'bash "$d/provision.sh"'
], '\n')

resource provision 'Microsoft.Compute/virtualMachines/extensions@2024-03-01' = {
  parent: vm
  name: 'provision'
  location: location
  properties: {
    publisher: 'Microsoft.Azure.Extensions'
    type: 'CustomScript'
    typeHandlerVersion: '2.1'
    autoUpgradeMinorVersion: true
    forceUpdateTag: uniqueString(provisionScript)
    protectedSettings: { script: base64(provisionScript) }
  }
}

// ---------------------------------------------------------------- identities for GitHub Actions (OIDC)

// Two identities, so the schedule cannot deploy and the deploy cannot start or
// stop the VM. Subjects name GitHub *environments*, not branches: the cron runs
// on the default branch (dev) while releases come from main.
resource deployIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-gh-jevtrader-deploy'
  location: location
}

resource deployFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: deployIdentity
  name: 'github-production'
  properties: {
    issuer: 'https://token.actions.githubusercontent.com'
    subject: 'repo:${githubRepo}:environment:production'
    audiences: ['api://AzureADTokenExchange']
  }
}

resource scheduleIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-gh-jevtrader-schedule'
  location: location
}

resource scheduleFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: scheduleIdentity
  name: 'github-market-hours'
  properties: {
    issuer: 'https://token.actions.githubusercontent.com'
    subject: 'repo:${githubRepo}:environment:market-hours'
    audiences: ['api://AzureADTokenExchange']
  }
}

// Read, start and deallocate on the VM, and nothing else.
resource startStopRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = {
  name: guid(resourceGroup().id, 'jevtrader-vm-start-deallocate')
  properties: {
    roleName: 'jevtrader VM start/deallocate (${resourceGroup().name})'
    description: 'Read, start and deallocate virtual machines in this resource group. Used by the market-hours workflow.'
    type: 'CustomRole'
    permissions: [
      {
        actions: [
          'Microsoft.Compute/virtualMachines/read'
          'Microsoft.Compute/virtualMachines/instanceView/read'
          'Microsoft.Compute/virtualMachines/start/action'
          'Microsoft.Compute/virtualMachines/deallocate/action'
        ]
      }
    ]
    assignableScopes: [resourceGroup().id]
  }
}

// ---------------------------------------------------------------- role assignments

resource deployAcrPush 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: acr
  name: guid(acr.id, deployIdentity.id, roles.acrPush)
  properties: {
    roleDefinitionId: roleId(roles.acrPush)
    principalId: deployIdentity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// Tag write on the VM only: the deploy workflow sets jev-image and nothing else.
resource deployTagWrite 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: vm
  name: guid(vm.id, deployIdentity.id, roles.tagContributor)
  properties: {
    roleDefinitionId: roleId(roles.tagContributor)
    principalId: deployIdentity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource scheduleStartStop 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: vm
  name: guid(vm.id, scheduleIdentity.id, startStopRole.id)
  properties: {
    roleDefinitionId: startStopRole.id
    principalId: scheduleIdentity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource vmAcrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: acr
  name: guid(acr.id, vm.id, roles.acrPull)
  properties: {
    roleDefinitionId: roleId(roles.acrPull)
    principalId: vm.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

resource vmSecretsUser 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: kv
  name: guid(kv.id, vm.id, roles.keyVaultSecretsUser)
  properties: {
    roleDefinitionId: roleId(roles.keyVaultSecretsUser)
    principalId: vm.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

resource vmBackupWriter 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: storage
  name: guid(storage.id, vm.id, roles.storageBlobDataContributor)
  properties: {
    roleDefinitionId: roleId(roles.storageBlobDataContributor)
    principalId: vm.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

resource operatorSecretsOfficer 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(operatorObjectId)) {
  scope: kv
  name: guid(kv.id, operatorObjectId, roles.keyVaultSecretsOfficer)
  properties: {
    roleDefinitionId: roleId(roles.keyVaultSecretsOfficer)
    principalId: operatorObjectId
    principalType: 'User'
  }
}

// ---------------------------------------------------------------- alerts

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' = {
  name: 'ag-jevtrader'
  location: 'global'
  properties: {
    groupShortName: 'jevtrader'
    enabled: true
    emailReceivers: [
      {
        name: 'owner'
        emailAddress: alertEmail
        useCommonAlertSchema: true
      }
    ]
  }
}

// Alerts only. Overspend does not stop anything.
resource budget 'Microsoft.Consumption/budgets@2023-05-01' = {
  name: 'budget-jevtrader'
  properties: {
    category: 'Cost'
    amount: budgetAmount
    timeGrain: 'Monthly'
    timePeriod: { startDate: budgetStartDate }
    notifications: {
      actual80: {
        enabled: true
        operator: 'GreaterThan'
        threshold: 80
        thresholdType: 'Actual'
        contactEmails: [alertEmail]
        contactGroups: [actionGroup.id]
      }
      actual100: {
        enabled: true
        operator: 'GreaterThan'
        threshold: 100
        thresholdType: 'Actual'
        contactEmails: [alertEmail]
        contactGroups: [actionGroup.id]
      }
      forecast100: {
        enabled: true
        operator: 'GreaterThan'
        threshold: 100
        thresholdType: 'Forecasted'
        contactEmails: [alertEmail]
        contactGroups: [actionGroup.id]
      }
    }
  }
}

// ---------------------------------------------------------------- outputs
// Identifiers only; none is a secret. They become the GitHub variables.

output tenantId string = subscription().tenantId
output subscriptionId string = subscription().subscriptionId
output resourceGroupName string = resourceGroup().name
output vmName string = vm.name
output acrName string = acr.name
output acrLoginServer string = acr.properties.loginServer
output keyVaultName string = kv.name
output storageAccountName string = storage.name
output publicIpAddress string = pip.properties.ipAddress
output deployClientId string = deployIdentity.properties.clientId
output scheduleClientId string = scheduleIdentity.properties.clientId
