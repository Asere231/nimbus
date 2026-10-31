// A small landing zone: a network with a security group, and a storage
// account whose name is derived from the resource group.
//
//   ./bin/nimbus start                      # terminal 1
//   az group create -n rg-bicep -l eastus   # terminal 2
//   az deployment group create -g rg-bicep --template-file examples/bicep/main.bicep
//
// Nimbus never sees this file. `az deployment group create` runs the Bicep
// compiler locally and sends the ARM JSON it produces, which is why main.json
// sits next to it: it is what actually gets deployed, and CI deploys it
// directly so the example works without the Bicep CLI installed.
//
// Regenerate main.json after editing this file:
//
//   az bicep build --file examples/bicep/main.bicep

@description('Prefix for every resource name.')
param prefix string = 'nimbus'

@description('Location for every resource. Defaults to the resource group\'s.')
param location string = resourceGroup().location

@description('How many subnets to create.')
@minValue(1)
@maxValue(8)
param subnetCount int = 2

// uniqueString is deterministic in Nimbus, but it is NOT Azure's algorithm,
// so this name differs between the two. Fine for a name that only has to be
// unique and stable; see the note in CLAUDE.md.
var storageName = toLower('st${uniqueString(resourceGroup().id)}')

resource nsg 'Microsoft.Network/networkSecurityGroups@2023-09-01' = {
  name: '${prefix}-nsg'
  location: location
  properties: {
    securityRules: [
      {
        name: 'allow-https'
        properties: {
          priority: 100
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourceAddressPrefix: '*'
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '443'
        }
      }
    ]
  }
}

resource vnet 'Microsoft.Network/virtualNetworks@2023-09-01' = {
  name: '${prefix}-vnet'
  location: location
  properties: {
    addressSpace: {
      addressPrefixes: [
        '10.20.0.0/16'
      ]
    }
    subnets: [
      for i in range(0, subnetCount): {
        name: 'snet-${i}'
        properties: {
          addressPrefix: '10.20.${i}.0/24'
          networkSecurityGroup: {
            id: nsg.id
          }
        }
      }
    ]
  }
}

resource storage 'Microsoft.Storage/storageAccounts@2023-01-01' = {
  name: storageName
  location: location
  sku: {
    name: 'Standard_LRS'
  }
  kind: 'StorageV2'
  properties: {
    supportsHttpsTrafficOnly: true
    minimumTlsVersion: 'TLS1_2'
  }
}

output vnetId string = vnet.id
output storageAccountName string = storage.name
output nsgRules int = length(nsg.properties.securityRules)
