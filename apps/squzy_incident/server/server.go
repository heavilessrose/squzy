package server

import (
	"context"
	"errors"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/google/uuid"
	apiPb "github.com/squzy/squzy_generated/generated/proto/v1"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"squzy/apps/squzy_incident/database"
	"squzy/apps/squzy_incident/expression"
)

type server struct {
	ruleDb  database.Database
	storage apiPb.StorageClient
	expr    expression.Expression
}

var (
	errNotValidRule = errors.New("rule is not valid")
)

func NewIncidentServer(storage apiPb.StorageClient, db database.Database) apiPb.IncidentServerServer {
	return &server{
		ruleDb:  db,
		storage: storage,
		expr:    expression.NewExpression(storage),
	}
}

func dbRuleToProto(rule *database.Rule) *apiPb.Rule {
	return &apiPb.Rule{
		Id:        rule.Id.Hex(),
		Rule:      rule.Rule,
		Name:      rule.Name,
		AutoClose: rule.AutoClose,
		OwnerType: rule.OwnerType,
		OwnerId:   rule.OwnerId.Hex(),
		Status:    rule.Status,
	}
}

func (s *server) ActivateRule(ctx context.Context, request *apiPb.RuleIdRequest) (*apiPb.Rule, error) {
	ruleId, err := primitive.ObjectIDFromHex(request.RuleId)
	if err != nil {
		return nil, err
	}

	rule, err := s.ruleDb.ActivateRule(ctx, ruleId)
	if err != nil {
		return nil, err
	}

	return dbRuleToProto(rule), nil
}

func (s *server) DeactivateRule(ctx context.Context, request *apiPb.RuleIdRequest) (*apiPb.Rule, error) {
	ruleId, err := primitive.ObjectIDFromHex(request.RuleId)
	if err != nil {
		return nil, err
	}

	rule, err := s.ruleDb.DeactivateRule(ctx, ruleId)
	if err != nil {
		return nil, err
	}

	return dbRuleToProto(rule), nil
}

func (s *server) CreateRule(ctx context.Context, request *apiPb.CreateRuleRequest) (*apiPb.Rule, error) {
	ownerId, err := primitive.ObjectIDFromHex(request.GetOwnerId())
	if err != nil {
		return nil, err
	}
	//	res, _ := s.ValidateRule(ctx, &apiPb.ValidateRuleRequest{ never return error
	res, _ := s.ValidateRule(ctx, &apiPb.ValidateRuleRequest{
		OwnerType: request.GetOwnerType(),
		Rule:      request.GetRule(),
	})
	if !res.IsValid {
		return nil, errNotValidRule
	}
	rule := &database.Rule{
		Id:        primitive.NewObjectID(),
		Rule:      request.GetRule(),
		Name:      request.GetName(),
		AutoClose: request.GetAutoClose(),
		OwnerType: request.GetOwnerType(),
		OwnerId:   ownerId,
		Status:    apiPb.RuleStatus_RULE_STATUS_ACTIVE,
	}
	err = s.ruleDb.SaveRule(ctx, rule)
	if err != nil {
		return nil, err
	}
	return dbRuleToProto(rule), err
}

func (s *server) GetRuleById(ctx context.Context, request *apiPb.RuleIdRequest) (*apiPb.Rule, error) {
	ruleId, err := primitive.ObjectIDFromHex(request.RuleId)
	if err != nil {
		return nil, err
	}

	rule, err := s.ruleDb.FindRuleById(ctx, ruleId)
	if err != nil {
		return nil, err
	}
	return dbRuleToProto(rule), nil
}

func (s *server) GetRulesByOwnerId(ctx context.Context, request *apiPb.GetRulesByOwnerIdRequest) (*apiPb.Rules, error) {
	ownerId, err := primitive.ObjectIDFromHex(request.GetOwnerId())
	if err != nil {
		return nil, err
	}

	dbRules, err := s.ruleDb.FindRulesByOwnerId(ctx, request.OwnerType, ownerId)
	rules := []*apiPb.Rule{}
	for _, rule := range dbRules {
		rules = append(rules, dbRuleToProto(rule))
	}
	return &apiPb.Rules{
		Rules: rules,
	}, err
}

func (s *server) RemoveRule(ctx context.Context, request *apiPb.RuleIdRequest) (*apiPb.Rule, error) {
	ruleId, err := primitive.ObjectIDFromHex(request.RuleId)
	if err != nil {
		return nil, err
	}

	rule, err := s.ruleDb.RemoveRule(ctx, ruleId)
	if err != nil {
		return nil, err
	}
	return dbRuleToProto(rule), nil
}

func (s *server) ValidateRule(ctx context.Context, request *apiPb.ValidateRuleRequest) (*apiPb.ValidateRuleResponse, error) {
	//Id the error handling will be added, add to the CreateRule
	err := s.expr.IsValid(request.OwnerType, request.Rule)
	if err != nil {
		return &apiPb.ValidateRuleResponse{
			IsValid: false,
			Error: &apiPb.ValidateRuleResponse_Error{
				Message: err.Error(),
			},
		}, nil
	}
	return &apiPb.ValidateRuleResponse{
		IsValid: true,
	}, nil
}

func (s *server) ProcessRecordFromStorage(ctx context.Context, request *apiPb.StorageRecord) (*empty.Empty, error) {
	ownerType, ownerId, err := getOwnerTypeAndId(request)
	if err != nil {
		return nil, err
	}
	rules, err := s.ruleDb.FindRulesByOwnerId(ctx, ownerType, ownerId)
	if err != nil {
		return nil, err
	}
	wasError := false
	for _, rule := range rules {
		if rule.Status != apiPb.RuleStatus_RULE_STATUS_ACTIVE {
			continue
		}
		wasIncident := s.expr.ProcessRule(ownerType, ownerId.Hex(), rule.Rule)
		// @TODO NIKITA HERE PROBLEM
		incident, err := s.storage.GetIncidentByRuleId(ctx, &apiPb.RuleIdRequest{
			RuleId: rule.Id.Hex(),
		})

		if err != nil {
			wasError = true
			continue
		}

		if isIncidentExist(incident) && isIncidentOpened(incident) && !wasIncident {
			if err := s.tryCloseIncident(ctx, rule.AutoClose, incident); err != nil {
				wasError = true
			}
			continue
		}

		if !isIncidentExist(incident) && wasIncident {
			incident = &apiPb.Incident{
				Status: apiPb.IncidentStatus_INCIDENT_STATUS_OPENED,
				RuleId: rule.Id.Hex(),
				Id:     uuid.New().String(),
				Histories: []*apiPb.Incident_HistoryItem{
					{
						Status:    apiPb.IncidentStatus_INCIDENT_STATUS_OPENED,
						Timestamp: ptypes.TimestampNow(),
					},
				},
			}
			if _, err := s.storage.SaveIncident(ctx, incident); err != nil {
				wasError = true
			}
			continue
		}
	}

	if wasError {
		return &empty.Empty{}, errors.New("WAS_ERROR_WHILE_RULE_PROCESSING")
	}
	return &empty.Empty{}, nil
}

func (s *server) CloseIncident(ctx context.Context, request *apiPb.IncidentIdRequest) (*apiPb.Incident, error) {
	return s.setStatus(ctx, request.GetIncidentId(), apiPb.IncidentStatus_INCIDENT_STATUS_CLOSED)
}

func (s *server) StudyIncident(ctx context.Context, request *apiPb.IncidentIdRequest) (*apiPb.Incident, error) {
	return s.setStatus(ctx, request.GetIncidentId(), apiPb.IncidentStatus_INCIDENT_STATUS_STUDIED)
}

func getOwnerTypeAndId(request *apiPb.StorageRecord) (apiPb.RuleOwnerType, primitive.ObjectID, error) {
	if request.GetSnapshot() != nil {
		ownerId, err := primitive.ObjectIDFromHex(request.GetSnapshot().Id)
		if err != nil {
			return 0, primitive.ObjectID{}, errors.New("ERROR_WRONG_ID")
		}
		return apiPb.RuleOwnerType_INCIDENT_OWNER_TYPE_SCHEDULER, ownerId, nil
	}
	if request.GetAgentMetric() != nil {
		ownerId, err := primitive.ObjectIDFromHex(request.GetAgentMetric().AgentId)
		if err != nil {
			return 0, primitive.ObjectID{}, errors.New("ERROR_WRONG_ID")
		}
		return apiPb.RuleOwnerType_INCIDENT_OWNER_TYPE_AGENT, ownerId, nil
	}
	if request.GetTransaction() != nil {
		ownerId, err := primitive.ObjectIDFromHex(request.GetTransaction().ApplicationId)
		if err != nil {
			return 0, primitive.ObjectID{}, errors.New("ERROR_WRONG_ID")
		}
		return apiPb.RuleOwnerType_INCIDENT_OWNER_TYPE_APPLICATION, ownerId, nil
	}
	return 0, primitive.ObjectID{}, errors.New("ERROR_NO_RECORD")
}

func isIncidentExist(incident *apiPb.Incident) bool {
	return incident != nil
}

func isIncidentOpened(incident *apiPb.Incident) bool {
	if incident == nil {
		return false
	}
	return incident.GetStatus() == apiPb.IncidentStatus_INCIDENT_STATUS_OPENED
}

func (s *server) tryCloseIncident(ctx context.Context, autoClose bool, incident *apiPb.Incident) error {
	if autoClose {
		_, err := s.setStatus(ctx, incident.GetId(), apiPb.IncidentStatus_INCIDENT_STATUS_CLOSED)
		return err
	}
	_, err := s.setStatus(ctx, incident.GetId(), apiPb.IncidentStatus_INCIDENT_STATUS_CAN_BE_CLOSED)
	return err
}

func (s *server) setStatus(ctx context.Context, id string, status apiPb.IncidentStatus) (*apiPb.Incident, error) {
	return s.storage.UpdateIncidentStatus(ctx, &apiPb.UpdateIncidentStatusRequest{
		IncidentId: id,
		Status:     status,
	})
}
