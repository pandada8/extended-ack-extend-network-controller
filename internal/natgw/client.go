package natgw

import (
	"context"
	"fmt"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	openapimodels "github.com/alibabacloud-go/darabonba-openapi/v2/models"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	credentials "github.com/aliyun/credentials-go/credentials"
)

const (
	SNATEntryStatusPending   = "Pending"
	SNATEntryStatusAvailable = "Available"
	SNATEntryStatusDeleting  = "Deleting"
)

type Client interface {
	EnsureSNATEntry(ctx context.Context, req EnsureSNATEntryRequest) (*SNATEntry, error)
	DeleteSNATEntry(ctx context.Context, req DeleteSNATEntryRequest) error
}

type EnsureSNATEntryRequest struct {
	NATGatewayID string
	SourceCIDR   string
	EIP          string
	EntryName    string
}

type DeleteSNATEntryRequest struct {
	NATGatewayID string
	SNATTableID  string
	SNATEntryID  string
	SourceCIDR   string
	EIP          string
}

type SNATEntry struct {
	ID         string
	NATGateway string
	SourceCIDR string
	EIP        string
	Status     string
	TableID    string
	RequestID  string
}

type AlibabaCloudClient struct {
	regionID string
	client   *vpc.Client
}

// NewAlibabaCloudClientFromEnv builds a VPC client using the Alibaba Cloud
// default credential provider chain. It supports (in order):
//
//  1. Environment variables (ALIBABA_CLOUD_ACCESS_KEY_ID/SECRET[/SECURITY_TOKEN]).
//  2. RAM role of an OIDC IdP (RRSA): ALIBABA_CLOUD_ROLE_ARN,
//     ALIBABA_CLOUD_OIDC_PROVIDER_ARN, ALIBABA_CLOUD_OIDC_TOKEN_FILE.
//  3. ~/.alibabacloud/credentials or aliyun-cli ~/.aliyun/config.json.
//  4. ECS instance RAM role (ALIBABA_CLOUD_ECS_METADATA).
//
// Refresh and STS rotation are handled by the credentials-go provider, so the
// returned client keeps working when temporary tokens expire.
func NewAlibabaCloudClientFromEnv(regionID string) (*AlibabaCloudClient, error) {
	cred, err := credentials.NewCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve alibaba cloud credential: %w", err)
	}

	config := &openapimodels.Config{}
	config.SetCredential(cred)
	config.SetRegionId(regionID)
	config.SetEndpoint(fmt.Sprintf("vpc.%s.aliyuncs.com", regionID))

	cli, err := vpc.NewClient((*openapi.Config)(config))
	if err != nil {
		return nil, err
	}

	return &AlibabaCloudClient{regionID: regionID, client: cli}, nil
}

func (c *AlibabaCloudClient) EnsureSNATEntry(ctx context.Context, req EnsureSNATEntryRequest) (*SNATEntry, error) {
	tableID, err := c.lookupSNATTableID(ctx, req.NATGatewayID)
	if err != nil {
		return nil, err
	}

	entry, err := c.findSNATEntry(ctx, tableID, req.NATGatewayID, req.SourceCIDR)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		if entry.EIP != req.EIP {
			return nil, fmt.Errorf("existing SNAT entry %s for %s uses eip %s, expected %s", entry.ID, req.SourceCIDR, entry.EIP, req.EIP)
		}
		return entry, nil
	}

	createReq := (&vpc.CreateSnatEntryRequest{}).
		SetRegionId(c.regionID).
		SetSnatTableId(tableID).
		SetSourceCIDR(req.SourceCIDR).
		SetSnatIp(req.EIP).
		SetSnatEntryName(req.EntryName)
	createResp, err := c.client.CreateSnatEntry(createReq)
	if err != nil {
		return nil, err
	}

	entry, err = c.findSNATEntry(ctx, tableID, req.NATGatewayID, req.SourceCIDR)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return &SNATEntry{
			NATGateway: req.NATGatewayID,
			SourceCIDR: req.SourceCIDR,
			EIP:        req.EIP,
			Status:     SNATEntryStatusPending,
			TableID:    tableID,
			RequestID:  stringValue(createResp.Body.RequestId),
		}, nil
	}
	entry.RequestID = stringValue(createResp.Body.RequestId)
	return entry, nil
}

func (c *AlibabaCloudClient) DeleteSNATEntry(ctx context.Context, req DeleteSNATEntryRequest) error {
	tableID := req.SNATTableID
	var err error
	if tableID == "" {
		tableID, err = c.lookupSNATTableID(ctx, req.NATGatewayID)
		if err != nil {
			return err
		}
	}

	entryID := req.SNATEntryID
	if entryID == "" {
		entry, err := c.findSNATEntry(ctx, tableID, req.NATGatewayID, req.SourceCIDR)
		if err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		if req.EIP != "" && entry.EIP != req.EIP {
			return fmt.Errorf("existing SNAT entry %s for %s uses eip %s, expected %s", entry.ID, req.SourceCIDR, entry.EIP, req.EIP)
		}
		if entry.Status == SNATEntryStatusDeleting {
			return nil
		}
		entryID = entry.ID
	}

	deleteReq := (&vpc.DeleteSnatEntryRequest{}).
		SetRegionId(c.regionID).
		SetSnatTableId(tableID).
		SetSnatEntryId(entryID)
	_, err = c.client.DeleteSnatEntry(deleteReq)
	return err
}

func (c *AlibabaCloudClient) lookupSNATTableID(ctx context.Context, natGatewayID string) (string, error) {
	_ = ctx
	resp, err := c.client.DescribeNatGateways((&vpc.DescribeNatGatewaysRequest{}).
		SetRegionId(c.regionID).
		SetNatGatewayId(natGatewayID).
		SetPageNumber(1).
		SetPageSize(1))
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Body == nil || resp.Body.NatGateways == nil || len(resp.Body.NatGateways.NatGateway) == 0 {
		return "", fmt.Errorf("nat gateway %s not found", natGatewayID)
	}
	gw := resp.Body.NatGateways.NatGateway[0]
	if gw.SnatTableIds == nil || len(gw.SnatTableIds.SnatTableId) == 0 || gw.SnatTableIds.SnatTableId[0] == nil {
		return "", fmt.Errorf("nat gateway %s does not expose an SNAT table", natGatewayID)
	}
	return *gw.SnatTableIds.SnatTableId[0], nil
}

func (c *AlibabaCloudClient) findSNATEntry(ctx context.Context, tableID, natGatewayID, sourceCIDR string) (*SNATEntry, error) {
	_ = ctx
	resp, err := c.client.DescribeSnatTableEntries((&vpc.DescribeSnatTableEntriesRequest{}).
		SetRegionId(c.regionID).
		SetSnatTableId(tableID).
		SetSourceCIDR(sourceCIDR).
		SetPageNumber(1).
		SetPageSize(50))
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil || resp.Body.SnatTableEntries == nil {
		return nil, nil
	}

	for _, item := range resp.Body.SnatTableEntries.SnatTableEntry {
		if item == nil || item.SourceCIDR == nil || *item.SourceCIDR != sourceCIDR {
			continue
		}
		entry := &SNATEntry{NATGateway: natGatewayID, SourceCIDR: sourceCIDR, TableID: tableID}
		if item.SnatEntryId != nil {
			entry.ID = *item.SnatEntryId
		}
		if item.SnatIp != nil {
			entry.EIP = *item.SnatIp
		}
		if item.Status != nil {
			entry.Status = *item.Status
		}
		return entry, nil
	}
	return nil, nil
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
