package cloudwatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana/pkg/infra/log"
	"golang.org/x/sync/errgroup"
)

type responseWrapper struct {
	DataResponse *backend.DataResponse
	RefId        string
}

func (e *cloudWatchExecutor) executeTimeSeriesQuery(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	plog.Debug("Executing time series query")
	resp := backend.NewQueryDataResponse()

	if len(req.Queries) == 0 {
		return nil, fmt.Errorf("request contains no queries")
	}

	// startTime and endTime are always the same for all queries
	startTime := req.Queries[0].TimeRange.From
	endTime := req.Queries[0].TimeRange.To
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid time range: start time must be before end time")
	}

	requestQueriesByRegion, err := e.parseQueries(req.Queries, startTime, endTime)
	if err != nil {
		return nil, err
	}

	if len(requestQueriesByRegion) == 0 {
		return backend.NewQueryDataResponse(), nil
	}

	resultChan := make(chan *responseWrapper, len(req.Queries))
	eg, ectx := errgroup.WithContext(ctx)
	for r, q := range requestQueriesByRegion {
		requestQueries := q
		region := r
		eg.Go(func() error {
			defer func() {
				if err := recover(); err != nil {
					plog.Error("Execute Get Metric Data Query Panic", "error", err, "stack", log.Stack(1))
					if theErr, ok := err.(error); ok {
						resultChan <- &responseWrapper{
							DataResponse: &backend.DataResponse{
								Error: theErr,
							},
						}
					}
				}
			}()

			client, err := e.getCWClient(region, req.PluginContext)
			if err != nil {
				return err
			}

			queries, err := e.transformRequestQueriesToCloudWatchQueries(requestQueries)
			if err != nil {
				return err
			}

			metricDataInput, err := e.buildMetricDataInput(startTime, endTime, queries)
			if err != nil {
				return err
			}

			mdo, err := e.executeRequest(ectx, client, metricDataInput)
			if err != nil {
				return err
			}

			responses, err := e.parseResponse(mdo, queries)
			if err != nil {
				return err
			}

			res, err := e.transformQueryResponsesToQueryResult(responses, requestQueries, startTime, endTime)
			if err != nil {
				return err
			}

			for refID, queryRes := range res {
				resultChan <- &responseWrapper{
					DataResponse: queryRes,
					RefId:        refID,
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		var awsErr awserr.RequestFailure
		if ok := errors.As(err, &awsErr); ok {
			dataResponse := backend.DataResponse{
				Error: fmt.Errorf("metric request error: %q", err),
			}
			resultChan <- &responseWrapper{
				DataResponse: &dataResponse,
			}
		}
		return nil, err
	}
	close(resultChan)

	for result := range resultChan {
		resp.Responses[result.RefId] = *result.DataResponse
	}

	return resp, nil
}
