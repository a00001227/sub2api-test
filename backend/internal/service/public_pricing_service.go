package service

import (
	"context"
	"sort"
)

// PublicModelPricingDTO is the response DTO for the public pricing endpoint.
// All price fields use per-token USD (same unit as LiteLLM and the rest of the billing engine).
type PublicModelPricingDTO struct {
	Model             string  `json:"model"`
	InputBase         float64 `json:"input_base"`
	OutputBase        float64 `json:"output_base"`
	GroupMultiplier   float64 `json:"group_multiplier"`
	ChannelMultiplier float64 `json:"channel_multiplier"`
	InputFinal        float64 `json:"input_final"`
	OutputFinal       float64 `json:"output_final"`
	SavingPercent     float64 `json:"saving_percent"`
}

// PublicPricingService aggregates model pricing data for the public-facing
// pricing endpoint. It is a read-only aggregation layer; it never mutates
// the billing engine or channel data.
type PublicPricingService struct {
	pricingService *PricingService
	groupRepo      GroupRepository
	channelService *ChannelService
}

// NewPublicPricingService constructs a PublicPricingService.
func NewPublicPricingService(
	pricingService *PricingService,
	groupRepo GroupRepository,
	channelService *ChannelService,
) *PublicPricingService {
	return &PublicPricingService{
		pricingService: pricingService,
		groupRepo:      groupRepo,
		channelService: channelService,
	}
}

// GetPublicPricing returns computed pricing for all models in the LiteLLM
// catalog that carry a non-zero input or output price.
//
// Multiplier resolution:
//   - group_multiplier  = the default public group's RateMultiplier (first active,
//     non-exclusive group ordered by SortOrder ASC then ID ASC); 1.0 when no
//     qualifying group exists.
//   - channel_multiplier = channel_input_price / litellm_input_price when the
//     default group's channel has an explicit per-token price for the model;
//     otherwise 1.0.
//   - input_final  = input_base  × group_multiplier × channel_multiplier
//   - output_final = output_base × group_multiplier × channel_multiplier
//   - saving_percent = (input_base - input_final) / input_base  (0 when base == 0)
func (s *PublicPricingService) GetPublicPricing(ctx context.Context) ([]PublicModelPricingDTO, error) {
	// 1. Resolve the default public group and its multiplier.
	groupMultiplier := 1.0
	var defaultGroupID *int64

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	// Pick the first active non-exclusive group ordered by SortOrder ASC then ID ASC.
	// Exclusive groups are subscription-gated and not representative of public pricing.
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].SortOrder != groups[j].SortOrder {
			return groups[i].SortOrder < groups[j].SortOrder
		}
		return groups[i].ID < groups[j].ID
	})
	for i := range groups {
		if !groups[i].IsExclusive {
			groupMultiplier = groups[i].RateMultiplier
			id := groups[i].ID
			defaultGroupID = &id
			break
		}
	}

	// 2. Snapshot all LiteLLM pricing data.
	allPricing := s.pricingService.ListAllPricing()

	// 3. Compute per-model DTOs.
	results := make([]PublicModelPricingDTO, 0, len(allPricing))
	for modelName, lp := range allPricing {
		if lp.InputCostPerToken == 0 && lp.OutputCostPerToken == 0 {
			continue
		}

		inputBase := lp.InputCostPerToken
		outputBase := lp.OutputCostPerToken

		// Resolve channel multiplier: 1.0 by default.
		channelMultiplier := 1.0
		if defaultGroupID != nil && s.channelService != nil {
			if chPricing := s.channelService.GetChannelModelPricing(ctx, *defaultGroupID, modelName); chPricing != nil {
				if chPricing.InputPrice != nil && inputBase > 0 {
					channelMultiplier = *chPricing.InputPrice / inputBase
				}
			}
		}

		inputFinal := inputBase * groupMultiplier * channelMultiplier
		outputFinal := outputBase * groupMultiplier * channelMultiplier

		savingPercent := 0.0
		if inputBase > 0 {
			savingPercent = (inputBase - inputFinal) / inputBase
		} else if outputBase > 0 {
			savingPercent = (outputBase - outputFinal) / outputBase
		}

		results = append(results, PublicModelPricingDTO{
			Model:             modelName,
			InputBase:         inputBase,
			OutputBase:        outputBase,
			GroupMultiplier:   groupMultiplier,
			ChannelMultiplier: channelMultiplier,
			InputFinal:        inputFinal,
			OutputFinal:       outputFinal,
			SavingPercent:     savingPercent,
		})
	}

	// 4. Stable sort: provider prefix ASC then model name ASC.
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Model < results[j].Model
	})

	return results, nil
}
