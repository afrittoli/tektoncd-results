package server

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/google/cel-go/cel"
	"github.com/google/uuid"

	resultscel "github.com/tektoncd/results/pkg/api/server/cel"
	dbmodel "github.com/tektoncd/results/pkg/api/server/db"
	"github.com/tektoncd/results/pkg/api/server/db/pagination"
	ppb "github.com/tektoncd/results/proto/pipeline/v1beta1/pipeline_go_proto"
	pb "github.com/tektoncd/results/proto/v1alpha1/results_go_proto"
	mask "go.chromium.org/luci/common/proto/mask"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"
)

const (
	listResultsDefaultPageSize = 50
	listResultsMaximumPageSize = 10000
)

// Server with implementation of API server
type Server struct {
	pb.UnimplementedResultsServer
	env *cel.Env
	gdb *gorm.DB
	db  *sql.DB
}

// CreateResult receives CreateResultRequest from clients and save it to local Sqlite Server.
func (s *Server) CreateResult(ctx context.Context, req *pb.CreateResultRequest) (*pb.Result, error) {
	r := req.GetResult()
	name := uuid.New().String()
	r.Name = fmt.Sprintf("%s/results/%s", req.GetParent(), name)

	// serialize data and insert it into database.
	b, err := proto.Marshal(r)
	if err != nil {
		log.Printf("result marshaling error: %v", err)
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Slightly confusing since this is CreateResult, but this maps better to
	// Records in the v1alpha2 API, so store this as a Record for
	// compatibility.
	record := &dbmodel.Record{
		Parent: req.GetParent(),
		// TODO: Require Records to be nested in Results. Since v1alpha1
		// results ~= records, allow parent-less records for now to allow
		// clients to continue working.
		ResultID: "",
		ID:       name,
		// This should be the parent-less name, but allow for now for compatibility.
		Name: r.Name,
		Data: b,
	}
	if err := s.gdb.WithContext(ctx).Create(record).Error; err != nil {
		return nil, err
	}

	return r, nil
}

// GetResult received GetResultRequest from users and return Result back to users
func (s *Server) GetResult(ctx context.Context, req *pb.GetResultRequest) (*pb.Result, error) {
	r, err := s.getResultByID(req.GetName())
	if err != nil {
		return nil, fmt.Errorf("failed to find a result: %w", err)
	}
	return r, nil
}

// UpdateResult receives Result and FieldMask from client and uses them to update records in local Sqlite Server.
func (s Server) UpdateResult(ctx context.Context, req *pb.UpdateResultRequest) (*pb.Result, error) {
	// Find corresponding Result in the database according to results_id.
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("failed to begin a transaction: %v", err)
		return nil, fmt.Errorf("failed to update a result: %w", err)
	}

	prev, err := s.getResultByID(req.GetName())
	if err != nil {
		return nil, fmt.Errorf("failed to find a result: %w", err)
	}

	r := proto.Clone(prev).(*pb.Result)
	// Update entire result if user do not specify paths
	if req.GetUpdateMask() == nil {
		r = req.GetResult()
	} else {
		// Merge Result from client into existing Result based on fieldmask.
		msk, err := mask.FromFieldMask(req.GetUpdateMask(), r, false, true)
		// Return NotFound error to client field is invalid
		if err != nil {
			log.Printf("failed to convert fieldmask to mask: %v", err)
			return nil, status.Errorf(codes.NotFound, "field in fieldmask not found in result")
		}
		if err := msk.Merge(req.GetResult(), r); err != nil {
			log.Printf("failed to merge new result into old result: %v", err)
			return nil, fmt.Errorf("failed to update result: %w", err)
		}
	}

	// Do any most-mask validation to make sure we are not mutating any immutable fields.
	if r.GetName() != prev.GetName() {
		return prev, status.Error(codes.InvalidArgument, "result name cannot be changed")
	}
	if r.GetCreatedTime() != prev.GetCreatedTime() {
		return prev, status.Error(codes.InvalidArgument, "created time cannot be changed")
	}

	// Write result back to database.
	b, err := proto.Marshal(r)
	if err != nil {
		log.Println("result marshaling error: ", err)
		return nil, fmt.Errorf("result marshaling error: %w", err)
	}
	statement, err := s.db.Prepare("UPDATE records SET data = ? WHERE name = ?")
	if err != nil {
		log.Printf("failed to update a existing result: %v", err)
		return nil, fmt.Errorf("failed to update a exsiting result: %w", err)
	}
	if _, err := statement.Exec(b, r.GetName()); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Printf("failed to rollback transaction: %v", err)
		}
		log.Printf("failed to execute update of a new result: %v", err)
		return nil, fmt.Errorf("failed to execute update of a new result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("failed to commit transaction: %v", err)
	}
	return r, nil
}

// DeleteResult receives DeleteResult request from users and delete Result in local Sqlite Server.
func (s Server) DeleteResult(ctx context.Context, req *pb.DeleteResultRequest) (*empty.Empty, error) {
	statement, err := s.db.Prepare("DELETE FROM records WHERE name = ?")
	if err != nil {
		log.Printf("failed to create delete statement: %v", err)
		return nil, fmt.Errorf("failed to create delete statement: %w", err)
	}
	results, err := statement.Exec(req.GetName())
	if err != nil {
		log.Printf("failed to execute delete statement: %v", err)
		return nil, fmt.Errorf("failed to execute delete statement: %w", err)
	}
	affect, err := results.RowsAffected()
	if err != nil {
		log.Printf("failed to retrieve results: %v", err)
		return nil, fmt.Errorf("failed to retrieve results: %w", err)
	}
	if affect == 0 {
		return nil, status.Errorf(codes.NotFound, "Result not found")
	}
	return nil, nil
}

// ListResultsResult receives a ListResultRequest from users and return to users a list of Results according to the query
func (s *Server) ListResultsResult(ctx context.Context, req *pb.ListResultsRequest) (*pb.ListResultsResponse, error) {
	// checks and refines the pageSize
	pageSize := int(req.GetPageSize())
	if pageSize < 0 {
		return nil, status.Error(codes.InvalidArgument, "PageSize should be greater than 0")
	} else if pageSize == 0 {
		pageSize = listResultsDefaultPageSize
	} else if pageSize > listResultsMaximumPageSize {
		pageSize = listResultsMaximumPageSize
	}

	// retrieve the ListPageIdentifier from PageToken
	var start string
	if t := req.GetPageToken(); t != "" {
		name, filter, err := pagination.DecodeToken(t)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("invalid PageToken: %v", err))
		}
		if req.GetFilter() != filter {
			return nil, status.Error(codes.InvalidArgument, "use a different CEL `filter` from the last page.")
		}
		start = name
	}

	prg, err := resultscel.ParseFilter(s.env, req.GetFilter())
	if err != nil {
		log.Printf("program construction error: %s", err)
		return nil, status.Errorf(codes.InvalidArgument, "Error occurred during filter checking step, no Results found for the query string due to invalid field, invalid function to evaluate filter or missing double quotes around field value, please try to enter a query with correct type again: %v", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	// always request one more result to know whether next page exists.
	results, err := getFilteredPaginatedResults(tx, pageSize+1, start, prg)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to commit the query transaction: %v", err))
	}

	if len(results) > pageSize {
		// there exists next page, generate the nextPageToken, and drop the last one of the results.
		nextResult := results[len(results)-1]
		results := results[:len(results)-1]
		if nextPageToken, err := pagination.EncodeToken(nextResult.GetName(), req.GetFilter()); err == nil {
			return &pb.ListResultsResponse{Results: results, NextPageToken: nextPageToken}, nil
		}
	}
	return &pb.ListResultsResponse{Results: results}, nil
}

// Check if the result can be reserved.
func matchCelFilter(r *pb.Result, prg cel.Program) (bool, error) {
	if prg == nil {
		return true, nil
	}
	for _, e := range r.Executions {
		// CEL requires non-nil values for protos, so default to 0 value if not
		// present in result.
		taskrun := &ppb.TaskRun{}
		if t := e.GetTaskRun(); t != nil {
			taskrun = t
		}
		pipelinerun := &ppb.PipelineRun{}
		if p := e.GetPipelineRun(); p != nil {
			pipelinerun = p
		}
		// We can't directly using e.GetTaskRun() and e.GetPipelineRun() here because the CEL doesn't work well with the nil pointer for proto types.
		out, _, err := prg.Eval(map[string]interface{}{
			"taskrun":     taskrun,
			"pipelinerun": pipelinerun,
		})
		if err != nil {
			log.Printf("failed to evaluate the expression: %v", err)
			return false, status.Errorf(codes.InvalidArgument, "Error occurred during filter evaluation step, no Results found for the query string due to invalid field, invalid function to evaluate filter or missing double quotes around field value, please try to enter a query with correct type again: %v", err)
		}
		if out.Value() == true {
			return true, nil
		}
	}
	return false, nil
}

// GetFilteredPaginatedResults aims to obtain a fixed size `pageSize` of results from the database, starting
// from the results with the identifier `startPI`, filtered by a compiled CEL program `prg`.
//
// In this function, we query the database multiple times and filter the queried results to
// comprise the final results.
//
// To minimize the query times, we introduce a variable `ratio` to indicate the retention rate
// after filtering a batch of results. The ratio of the queried batch is:
//             ratio = remained_results_size/batch_size.
//
// The batchSize depends on the `ratio` of the previous batch and the `pageSize`:
//                  batchSize = pageSize/last_ratio
// The less the previous ratio is, the bigger the upcoming batch_size is. Then the queried time
// is significantly decreased.
func getFilteredPaginatedResults(tx *sql.Tx, pageSize int, start string, prg cel.Program) (results []*pb.Result, err error) {
	var lastName string
	//var ratio float32 = 1
	batcher := pagination.NewBatcher(pageSize, listResultsDefaultPageSize, listResultsMaximumPageSize)
	for len(results) < pageSize {
		// If didn't get enought results.
		batchSize := batcher.Next()
		var rows *sql.Rows
		if lastName == "" {
			if start != "" {
				rows, err = tx.Query("SELECT name, data FROM records WHERE name >= ? ORDER BY name LIMIT ? ", start, batchSize)
			} else {
				rows, err = tx.Query("SELECT name, data FROM records ORDER BY name LIMIT ?", batchSize)
			}
		} else {
			rows, err = tx.Query("SELECT name, data FROM records WHERE name > ? ORDER BY name LIMIT ? ", lastName, batchSize)
		}
		if err != nil {
			log.Printf("failed to query on database: %v", err)
			return nil, status.Errorf(codes.Internal, "failed to query results: %v", err)
		}

		var (
			batchGot     int // number of items returned from the query. Always <= less than batchSize.
			batchMatched int // number of items returned from the query that satisfy the filter condition. Always <= batchGot.
		)
		for rows.Next() {
			batchGot++
			var b []byte
			if err := rows.Scan(&lastName, &b); err != nil {
				log.Printf("failed to scan a row in query results: %v", err)
				return nil, status.Errorf(codes.Internal, "failed to read result data: %v", err)
			}
			r := &pb.Result{}
			if err := proto.Unmarshal(b, r); err != nil {
				log.Printf("unmarshaling error: %v", err)
				return nil, status.Errorf(codes.Internal, "failed to parse result data: %v", err)
			}
			// filter the results one by one
			if ok, _ := matchCelFilter(r, prg); ok {
				batchMatched++
				results = append(results, r)
				if len(results) >= pageSize {
					break
				}
			}
		}
		if batchGot < batchSize {
			// No more data in database.
			break
		}
		// update batcher to determine the next batch size.
		batcher.Update(batchMatched, batchGot)
	}
	return results, nil
}

// GetResultByID is the helper function to get a Result by results_id
func (s Server) getResultByID(name string) (*pb.Result, error) {
	rows, err := s.db.Query("SELECT data FROM records WHERE name = ?", name)
	if err != nil {
		log.Printf("failed to query on database: %v", err)
		return nil, fmt.Errorf("failed to query on a result: %w", err)
	}
	result := &pb.Result{}
	rowNum := 0
	for rows.Next() {
		var b []byte
		rowNum++
		if rowNum >= 2 {
			log.Println("Warning: multiple rows found")
			break
		}
		if err := rows.Scan(&b); err != nil {
			log.Printf("error scanning rows: %v", err)
			return nil, fmt.Errorf("error scanning rows: %w", err)
		}
		if err := proto.Unmarshal(b, result); err != nil {
			log.Printf("unmarshaling error: %v", err)
			return nil, fmt.Errorf("failed to unmarshal result: %w", err)
		}
	}
	if rowNum == 0 {
		return nil, status.Error(codes.NotFound, "result not found")
	}
	return result, nil
}

// New set up environment for the api server
func New(gdb *gorm.DB) (*Server, error) {
	env, err := resultscel.NewEnv()
	if err != nil {
		log.Fatalf("failed to create environment for filter: %v", err)
	}
	db, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	srv := &Server{
		gdb: gdb,
		db:  db,
		env: env,
	}
	return srv, nil
}
