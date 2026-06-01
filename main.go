package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/dns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "nuon")
		nuonID := cfg.Require("nuon_id")
		projectID := cfg.Require("project_id")
		region := cfg.Require("region")
		enableDNSStr := cfg.Get("enable_nuon_dns")
		publicRoot := cfg.Get("public_root_domain")
		internalRoot := cfg.Get("internal_root_domain")
		network := cfg.Get("network")

		enableDNS := enableDNSStr == "true" || enableDNSStr == "1"

		labels := pulumi.StringMap{
			"nuon-id":      pulumi.String(nuonID),
			"managed-by":   pulumi.String("nuon"),
			"sandbox-name": pulumi.String("gcp-min"),
		}
		mergeSanitizedFromCfg(labels, cfg, "labels")
		mergeSanitizedFromCfg(labels, cfg, "tags")
		mergeSanitizedFromCfg(labels, cfg, "additional_tags")

		// Use an existing VPC when one is provided (matches the install stack's
		// network), otherwise create a new one for the install.
		var networkName, networkSelfLink, networkURL pulumi.StringInput
		if network != "" {
			existing, err := compute.LookupNetwork(ctx, &compute.LookupNetworkArgs{
				Name:    network,
				Project: &projectID,
			})
			if err != nil {
				return fmt.Errorf("look up existing vpc %q: %w", network, err)
			}
			networkName = pulumi.String(existing.Name)
			networkSelfLink = pulumi.String(existing.SelfLink)
			networkURL = pulumi.String(existing.SelfLink)
		} else {
			net, err := compute.NewNetwork(ctx, "main", &compute.NetworkArgs{
				Project:               pulumi.String(projectID),
				Name:                  pulumi.Sprintf("%s-vpc", nuonID),
				AutoCreateSubnetworks: pulumi.Bool(true),
			})
			if err != nil {
				return fmt.Errorf("create vpc: %w", err)
			}
			networkName = net.Name
			networkSelfLink = net.SelfLink
			networkURL = net.SelfLink
		}

		repo, err := artifactregistry.NewRepository(ctx, "main", &artifactregistry.RepositoryArgs{
			Project:      pulumi.String(projectID),
			Location:     pulumi.String(region),
			RepositoryId: pulumi.String(nuonID),
			Format:       pulumi.String("DOCKER"),
			Labels:       labels,
			CleanupPolicies: artifactregistry.RepositoryCleanupPolicyArray{
				&artifactregistry.RepositoryCleanupPolicyArgs{
					Id:     pulumi.String("keep-recent"),
					Action: pulumi.String("KEEP"),
					MostRecentVersions: &artifactregistry.RepositoryCleanupPolicyMostRecentVersionsArgs{
						KeepCount: pulumi.Int(10),
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("create artifact registry repo: %w", err)
		}

		var publicZone *dns.ManagedZone
		if enableDNS && publicRoot != "" {
			publicZone, err = dns.NewManagedZone(ctx, "public", &dns.ManagedZoneArgs{
				Project:      pulumi.String(projectID),
				Name:         pulumi.Sprintf("%s-public", nuonID),
				DnsName:      pulumi.Sprintf("%s.", publicRoot),
				Labels:       labels,
				Description:  pulumi.Sprintf("Public DNS zone for install %s", nuonID),
				ForceDestroy: pulumi.Bool(true),
			})
			if err != nil {
				return fmt.Errorf("create public dns zone: %w", err)
			}
		}

		var internalZone *dns.ManagedZone
		if internalRoot != "" {
			internalZone, err = dns.NewManagedZone(ctx, "internal", &dns.ManagedZoneArgs{
				Project:      pulumi.String(projectID),
				Name:         pulumi.Sprintf("%s-internal", nuonID),
				DnsName:      pulumi.Sprintf("%s.", internalRoot),
				Visibility:   pulumi.String("private"),
				Labels:       labels,
				Description:  pulumi.Sprintf("Internal DNS zone for install %s", nuonID),
				ForceDestroy: pulumi.Bool(true),
				PrivateVisibilityConfig: &dns.ManagedZonePrivateVisibilityConfigArgs{
					Networks: dns.ManagedZonePrivateVisibilityConfigNetworkArray{
						&dns.ManagedZonePrivateVisibilityConfigNetworkArgs{
							NetworkUrl: networkURL,
						},
					},
				},
			})
			if err != nil {
				return fmt.Errorf("create internal dns zone: %w", err)
			}
		}

		ctx.Export("account", pulumi.Map{
			"project_id": pulumi.String(projectID),
			"region":     pulumi.String(region),
		})

		ctx.Export("vpc", pulumi.Map{
			"network":           networkName,
			"network_self_link": networkSelfLink,
		})

		ctx.Export("gar", pulumi.Map{
			"repository_id": repo.RepositoryId,
			"repository_url": pulumi.Sprintf("%s-docker.pkg.dev/%s/%s",
				region, projectID, nuonID),
			"registry_url": pulumi.Sprintf("%s-docker.pkg.dev", region),
		})

		ctx.Export("nuon_dns", buildDNSOutput(enableDNS, internalRoot, publicZone, internalZone))

		return nil
	})
}

var labelSanitizer = regexp.MustCompile(`[/._]`)

func sanitizeLabel(s string) string {
	out := strings.ToLower(labelSanitizer.ReplaceAllString(s, "-"))
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

func mergeSanitizedFromCfg(dst pulumi.StringMap, cfg *config.Config, key string) {
	var src map[string]string
	if err := cfg.TryObject(key, &src); err != nil || len(src) == 0 {
		return
	}
	for k, v := range src {
		dst[sanitizeLabel(k)] = pulumi.String(sanitizeLabel(v))
	}
}

func buildDNSOutput(enableDNS bool, internalRoot string, public, internal *dns.ManagedZone) pulumi.Map {
	emptyDomain := pulumi.Map{
		"zone_id":     pulumi.String(""),
		"name":        pulumi.String(""),
		"nameservers": pulumi.StringArray{},
	}

	publicDomain := emptyDomain
	if public != nil {
		publicDomain = pulumi.Map{
			"zone_id":     public.ManagedZoneId,
			"name":        public.DnsName.ApplyT(trimTrailingDot).(pulumi.StringOutput),
			"nameservers": public.NameServers,
		}
	}

	internalDomain := emptyDomain
	if internal != nil && internalRoot != "" {
		internalDomain = pulumi.Map{
			"zone_id":     internal.ManagedZoneId,
			"name":        internal.DnsName.ApplyT(trimTrailingDot).(pulumi.StringOutput),
			"nameservers": internal.NameServers,
		}
	}

	return pulumi.Map{
		"enabled":         pulumi.Bool(enableDNS),
		"public_domain":   publicDomain,
		"internal_domain": internalDomain,
	}
}

func trimTrailingDot(s string) string {
	return strings.TrimSuffix(s, ".")
}
