package aws

import (
	"encoding/xml"
	"net/http"
	"strings"
)

// DescribeInstanceTypes.
//
// Called AFTER the instance is created, as part of the provider's
// read-back -- not before it, to validate anything. Measured against aws
// provider 5.100.0: the apply ran the instance, waited for it to reach
// running, and only then failed on this action.
//
// An unknown type is ANSWERED, not refused. The real list is hundreds of
// entries, changes every few weeks, and nothing in Nimbus limits an
// instance's CPU or memory to match one -- so refusing an unmodelled type
// would fail an apply over numbers that mean nothing locally. The same
// rule as an unknown describe filter: answering can only ever be too
// generous, and the client narrows it.
//
// The numbers are derived from the type's NAME where it is recognisable
// (m5.4xlarge really does report more than t3.micro) so a reader sees
// something plausible rather than a constant. It is a convenience, not a
// catalogue.

type vCPUInfo struct {
	DefaultVCpus          int   `xml:"defaultVCpus"`
	DefaultCores          int   `xml:"defaultCores"`
	DefaultThreadsPerCore int   `xml:"defaultThreadsPerCore"`
	ValidCores            []int `xml:"validCores>item,omitempty"`
	ValidThreadsPerCore   []int `xml:"validThreadsPerCore>item,omitempty"`
}

type memoryInfo struct {
	SizeInMiB int `xml:"sizeInMiB"`
}

type processorInfo struct {
	SupportedArchitectures []string `xml:"supportedArchitectures>item"`
	SustainedClockSpeedGhz string   `xml:"sustainedClockSpeedInGhz"`
}

type networkInfo struct {
	NetworkPerformance           string `xml:"networkPerformance"`
	MaximumNetworkInterfaces     int    `xml:"maximumNetworkInterfaces"`
	Ipv4AddressesPerInterface    int    `xml:"ipv4AddressesPerInterface"`
	Ipv6AddressesPerInterface    int    `xml:"ipv6AddressesPerInterface"`
	Ipv6Supported                bool   `xml:"ipv6Supported"`
	EnaSupport                   string `xml:"enaSupport"`
	EncryptionInTransitSupported bool   `xml:"encryptionInTransitSupported"`
}

type ebsInfo struct {
	EbsOptimizedSupport string `xml:"ebsOptimizedSupport"`
	EncryptionSupport   string `xml:"encryptionSupport"`
	NvmeSupport         string `xml:"nvmeSupport"`
}

type instanceTypeInfo struct {
	InstanceType                  string        `xml:"instanceType"`
	CurrentGeneration             bool          `xml:"currentGeneration"`
	FreeTierEligible              bool          `xml:"freeTierEligible"`
	SupportedUsageClasses         []string      `xml:"supportedUsageClasses>item"`
	SupportedRootDeviceTypes      []string      `xml:"supportedRootDeviceTypes>item"`
	SupportedVirtualizationTypes  []string      `xml:"supportedVirtualizationTypes>item"`
	BareMetal                     bool          `xml:"bareMetal"`
	Hypervisor                    string        `xml:"hypervisor"`
	ProcessorInfo                 processorInfo `xml:"processorInfo"`
	VCPUInfo                      vCPUInfo      `xml:"vCpuInfo"`
	MemoryInfo                    memoryInfo    `xml:"memoryInfo"`
	InstanceStorageSupported      bool          `xml:"instanceStorageSupported"`
	EbsInfo                       ebsInfo       `xml:"ebsInfo"`
	NetworkInfo                   networkInfo   `xml:"networkInfo"`
	BurstablePerformanceSupported bool          `xml:"burstablePerformanceSupported"`
	DedicatedHostsSupported       bool          `xml:"dedicatedHostsSupported"`
	AutoRecoverySupported         bool          `xml:"autoRecoverySupported"`
	SupportedBootModes            []string      `xml:"supportedBootModes>item"`
}

type describeInstanceTypesResponse struct {
	XMLName   xml.Name           `xml:"DescribeInstanceTypesResponse"`
	Xmlns     string             `xml:"xmlns,attr"`
	RequestID string             `xml:"requestId"`
	Types     []instanceTypeInfo `xml:"instanceTypeSet>item"`
}

func (s *Server) describeInstanceTypes(w http.ResponseWriter, p params) *apiError {
	wanted := p.list("InstanceType", "")
	if len(wanted) == 0 {
		// Without a list the request is "tell me about everything", which
		// here means the types instances have actually been launched with.
		seen := map[string]bool{}
		for _, i := range list[instance](s, typeInstance) {
			if !seen[i.InstanceType] {
				seen[i.InstanceType] = true
				wanted = append(wanted, i.InstanceType)
			}
		}
		if len(wanted) == 0 {
			wanted = []string{"t3.micro"}
		}
	}

	out := make([]instanceTypeInfo, 0, len(wanted))
	for _, t := range wanted {
		out = append(out, instanceTypeDetails(t))
	}
	writeXML(w, 200, describeInstanceTypesResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), Types: out,
	})
	return nil
}

// sizeFactors maps the size half of an instance type name to how many
// "units" it is. AWS's own naming is this regular, which is why the
// numbers can be derived rather than tabulated.
var sizeFactors = map[string]int{
	"nano": 1, "micro": 1, "small": 1, "medium": 1,
	"large": 2, "xlarge": 4, "2xlarge": 8, "3xlarge": 12, "4xlarge": 16,
	"6xlarge": 24, "8xlarge": 32, "9xlarge": 36, "12xlarge": 48,
	"16xlarge": 64, "18xlarge": 72, "24xlarge": 96, "32xlarge": 128,
	"metal": 96,
}

// memoryPerVCPU is the family's rough memory-to-CPU ratio in MiB. t and m
// are general purpose, c is compute-optimised, r and x memory-optimised.
func memoryPerVCPU(family string) int {
	switch {
	case strings.HasPrefix(family, "x"):
		return 16 * 1024
	case strings.HasPrefix(family, "r"), strings.HasPrefix(family, "z"):
		return 8 * 1024
	case strings.HasPrefix(family, "c"):
		return 2 * 1024
	default:
		return 4 * 1024
	}
}

// instanceTypeDetails derives a plausible description of any type name,
// including one nobody has heard of.
func instanceTypeDetails(name string) instanceTypeInfo {
	family, size := name, ""
	if dot := strings.Index(name, "."); dot >= 0 {
		family, size = name[:dot], name[dot+1:]
	}

	units, known := sizeFactors[size]
	if !known {
		units = 2 // an unrecognised size is treated as "large"
	}
	vcpus := units
	if units < 2 {
		vcpus = 2 // nano through medium are all 2 vCPUs on current families
	}
	memory := vcpus * memoryPerVCPU(family) / 2
	if units == 1 {
		// nano/micro/small/medium differ in memory, not in vCPUs.
		memory = map[string]int{"nano": 512, "micro": 1024, "small": 2048, "medium": 4096}[size]
		if memory == 0 {
			memory = 1024
		}
	}

	arch := "x86_64"
	// A "g" immediately before the dot is Graviton: m6g, c7g, t4g.
	if strings.HasSuffix(family, "g") && len(family) > 2 {
		arch = "arm64"
	}
	burstable := strings.HasPrefix(family, "t")

	return instanceTypeInfo{
		InstanceType:                 name,
		CurrentGeneration:            true,
		FreeTierEligible:             name == "t2.micro" || name == "t3.micro",
		SupportedUsageClasses:        []string{"on-demand", "spot"},
		SupportedRootDeviceTypes:     []string{"ebs"},
		SupportedVirtualizationTypes: []string{"hvm"},
		Hypervisor:                   "nitro",
		ProcessorInfo: processorInfo{
			SupportedArchitectures: []string{arch},
			SustainedClockSpeedGhz: "2.5",
		},
		VCPUInfo: vCPUInfo{
			DefaultVCpus: vcpus,
			// Two threads per core on x86, one on Graviton -- which is what
			// the provider's cpu_options is built from, so a zero here
			// would make it plan a change it cannot apply.
			DefaultCores:          coresFor(vcpus, arch),
			DefaultThreadsPerCore: threadsFor(arch),
		},
		MemoryInfo:               memoryInfo{SizeInMiB: memory},
		InstanceStorageSupported: false,
		EbsInfo:                  ebsInfo{EbsOptimizedSupport: "default", EncryptionSupport: "supported", NvmeSupport: "required"},
		NetworkInfo: networkInfo{
			NetworkPerformance:        "Up to 5 Gigabit",
			MaximumNetworkInterfaces:  3,
			Ipv4AddressesPerInterface: 6,
			Ipv6AddressesPerInterface: 6,
			Ipv6Supported:             true,
			EnaSupport:                "required",
		},
		BurstablePerformanceSupported: burstable,
		DedicatedHostsSupported:       !burstable,
		AutoRecoverySupported:         true,
		SupportedBootModes:            []string{"legacy-bios", "uefi"},
	}
}

func threadsFor(arch string) int {
	if arch == "arm64" {
		return 1
	}
	return 2
}

func coresFor(vcpus int, arch string) int {
	cores := vcpus / threadsFor(arch)
	if cores < 1 {
		cores = 1
	}
	return cores
}
