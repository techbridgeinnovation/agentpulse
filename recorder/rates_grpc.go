package recorder

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

const (
	// ratePageSize is the page the rate card is read in, at the cap metering enforces.
	ratePageSize = 1000

	// maxRatePages bounds how many pages one refresh will follow.
	//
	// A rate card is a maintained price list of tens of entries, so this is far above what any refresh reads. It exists so a paging bug on either side cannot turn one background fetch into an endless loop holding a growing slice inside the host agent's process.
	maxRatePages = 10
)

// GRPCRateSource reads the rate card from the metering service.
type GRPCRateSource struct {
	client pb.PriceableUnitsServiceClient
}

// NewGRPCRateSource wraps an existing connection.
//
// The connection is the caller's, following the same pattern as NewGRPCSink and NewGRPCDecider: an agent already has one to the platform, and dialling a second would double the sockets for no reason. The recorder never closes it.
func NewGRPCRateSource(conn grpc.ClientConnInterface) *GRPCRateSource {
	return &GRPCRateSource{client: pb.NewPriceableUnitsServiceClient(conn)}
}

// ListRates reads the whole rate card, following every page.
//
// All of it rather than the entries for one model: which models an agent calls is not known here, a delegating agent calls whichever its sub-agents are configured with, and the whole card is small enough that reading it once every refresh costs less than working out what to ask for.
func (s *GRPCRateSource) ListRates(ctx context.Context) ([]*pb.PriceableUnit, error) {
	var units []*pb.PriceableUnit
	token := ""

	for range maxRatePages {
		resp, err := s.client.ListPriceableUnits(ctx, &pb.ListPriceableUnitsRequest{
			PageSize:  ratePageSize,
			PageToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("listing the rate card: %w", err)
		}

		units = append(units, resp.GetPriceableUnits()...)

		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}

	return units, nil
}
