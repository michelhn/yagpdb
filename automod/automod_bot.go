package automod

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/dstate"
	"github.com/jonas747/yagpdb/automod/models"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/bot/eventsystem"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/pubsub"
	"github.com/sirupsen/logrus"
	"github.com/volatiletech/null"
	"github.com/volatiletech/sqlboiler/boil"
	"github.com/volatiletech/sqlboiler/queries/qm"
	"sort"
)

const PubSubEvtCleaCache = "automod_2_clear_guild_cache"

func (p *Plugin) BotInit() {
	eventsystem.AddHandler(p.handleMessageCreate, eventsystem.EventMessageCreate)
	eventsystem.AddHandler(p.handleGuildMemberUpdate, eventsystem.EventGuildMemberUpdate)

	pubsub.AddHandler(PubSubEvtCleaCache, func(evt *pubsub.Event) {
		gs := bot.State.Guild(true, evt.TargetGuildInt)
		if gs == nil {
			return
		}

		gs.UserCacheDel(true, CacheKeyRulesets)
		gs.UserCacheDel(true, CacheKeyLists)

		logrus.Println("cleared automod cache for ", gs.ID)
	}, nil)
}

func (p *Plugin) handleMessageCreate(evt *eventsystem.EventData) {
	m := evt.MessageCreate()

	cs := bot.State.Channel(true, m.ChannelID)
	if cs == nil || cs.Guild == nil {
		return
	}

	ms, err := bot.GetMember(m.GuildID, m.Author.ID)
	if err != nil {
		logrus.WithError(err).Error("automod failed fetching member")
		return
	}

	stripped := common.StripMarkdown(m.Message.Content)
	p.CheckTriggers(nil, ms, m.Message, cs, func(trig *ParsedPart) (activated bool, err error) {
		cast, ok := trig.Part.(MessageTrigger)
		if !ok {
			return
		}

		return cast.CheckMessage(ms, cs, m.Message, stripped, trig.ParsedSettings)
	})
}

func (p *Plugin) checkViolationTriggers(ctxData *TriggeredRuleData, violationName string) {
	// reset context data
	ctxData.ActivatedTriggers = nil
	ctxData.CurrentRule = nil
	ctxData.TriggeredRules = nil

	if ctxData.RecursionCounter > 2 {
		logrus.WithField("guild", ctxData.GS.ID).Warn("automod stopped infinite recursion")
		return
	}

	rulesets, err := p.FetchGuildRulesets(ctxData.GS)
	if err != nil {
		logrus.WithError(err).WithField("guild", ctxData.GS.ID).Error("failed fetching guild rulesets")
		return
	}

	if len(rulesets) < 1 {
		return
	}

	// retrieve users violations
	userViolations, err := models.AutomodViolations(qm.Where("guild_id = ? AND user_id = ? AND name = ?", ctxData.GS.ID, ctxData.MS.ID, violationName)).AllG(context.Background())
	if err != nil {
		logrus.WithError(err).Error("automod failed retrieving user violations")
		return
	}

	for _, rs := range rulesets {
		if !rs.RSModel.Enabled {
			continue
		}

		// Check for triggered rules in this ruleset
		ctxData.Ruleset = rs
		if !p.CheckConditions(ctxData, rs.ParsedConditions) {
			continue
		}

		var activatedTriggers []*ParsedPart

		for _, rule := range rs.Rules {
			ctxData.CurrentRule = rule

			// Check conditions
			if !p.CheckConditions(ctxData, rule.Conditions) {
				continue
			}

			// check if one of the triggers should be activated
			for _, trig := range rule.Triggers {
				violationTrigger, ok := trig.Part.(ViolationListener)
				if !ok {
					continue
				}

				tDataCast := trig.ParsedSettings.(*ViolationsTriggerData)
				if tDataCast.Name != violationName {
					continue
				}

				matched, err := violationTrigger.CheckUser(ctxData, userViolations, trig.ParsedSettings, false)
				if err != nil {
					logrus.WithError(err).WithField("part_id", trig.RuleModel.ID).Error("failed checking violations trigger")
					continue
				}

				if matched {
					activatedTriggers = append(activatedTriggers, trig)
					break
				}
			}
		}

		if len(activatedTriggers) < 1 {
			// no matches :(
			continue
		}

		// sort them in order from highest to lowest treshold
		sort.Slice(activatedTriggers, func(i, j int) bool {
			d1 := activatedTriggers[i].ParsedSettings.(*ViolationsTriggerData)
			d2 := activatedTriggers[j].ParsedSettings.(*ViolationsTriggerData)

			return d1.Treshold > d2.Treshold
		})

		// do a second pass with the triggers sorted, incase only the highest should be triggered
		finalActivatedTriggers := make([]*ParsedPart, 0, len(activatedTriggers))
		finalTriggeredRules := make([]*ParsedRule, 0, len(activatedTriggers))

		triggeredOne := false
		for _, t := range activatedTriggers {
			ctxData.CurrentRule = t.ParentRule

			violationTrigger := t.Part.(ViolationListener)
			matched, err := violationTrigger.CheckUser(ctxData, userViolations, t.ParsedSettings, triggeredOne)
			if err != nil {
				logrus.WithError(err).WithField("part_id", t.RuleModel.ID).Error("failed checking violations trigger")
				continue
			}

			if matched {
				finalActivatedTriggers = append(finalActivatedTriggers, t)
				finalTriggeredRules = append(finalTriggeredRules, t.ParentRule)
				triggeredOne = true
			}
		}

		cClone := ctxData.Clone()
		cClone.Ruleset = rs
		cClone.TriggeredRules = finalTriggeredRules
		cClone.ActivatedTriggers = finalActivatedTriggers
		cClone.CurrentRule = nil

		go p.RulesetRulesTriggered(cClone)
		logrus.Println("Triggered violation rules: ", finalTriggeredRules, ctxData.GS.ID)
	}
}

func (p *Plugin) handleGuildMemberUpdate(evt *eventsystem.EventData) {
	evtData := evt.GuildMemberUpdate()
	ms, err := bot.GetMember(evtData.GuildID, evtData.User.ID)
	if err != nil || ms == nil {
		return
	}

	if ms.Nick == "" {
		return
	}

	p.checkNickname(ms)
}

func (p *Plugin) checkNickname(ms *dstate.MemberState) {
	p.CheckTriggers(nil, ms, nil, nil, func(trig *ParsedPart) (activated bool, err error) {
		cast, ok := trig.Part.(NicknameListener)
		if !ok {
			return false, nil
		}

		return cast.CheckNickname(ms, trig.ParsedSettings)
	})
}

func (p *Plugin) CheckTriggers(rulesets []*ParsedRuleset, ms *dstate.MemberState, msg *discordgo.Message, cs *dstate.ChannelState, checkF func(trp *ParsedPart) (activated bool, err error)) {
	if rulesets == nil {
		var err error
		rulesets, err = p.FetchGuildRulesets(ms.Guild)
		if err != nil {
			logrus.WithError(err).WithField("guild", ms.Guild.ID).Error("automod: failed fetching triggers")
			return
		}

		if len(rulesets) < 1 {
			return
		}
	}

	for _, rs := range rulesets {
		if !rs.RSModel.Enabled {
			continue
		}

		// Check for triggered rules in this ruleset
		var triggeredRules []*ParsedRule
		var activatedTriggers []*ParsedPart

		for _, rule := range rs.Rules {

			for _, trig := range rule.Triggers {

				activated, err := checkF(trig)
				if err != nil {
					logrus.WithError(err).WithField("part_id", trig.RuleModel.ID).Error("failed checking trigger")
					continue
				}

				if activated {
					triggeredRules = append(triggeredRules, rule)
					activatedTriggers = append(activatedTriggers, trig)
					break
				}

			}

		}

		if len(triggeredRules) < 1 {
			// no matches :(
			continue
		}

		ctxData := &TriggeredRuleData{
			MS:      ms,
			CS:      cs,
			GS:      ms.Guild,
			Plugin:  p,
			Ruleset: rs,

			TriggeredRules:    triggeredRules,
			ActivatedTriggers: activatedTriggers,
			Message:           msg,
		}

		if ctxData.Message != nil {
			ctxData.StrippedMessageContent = common.StripMarkdown(ctxData.Message.Content)
		}

		go p.RulesetRulesTriggered(ctxData)
		logrus.Println("Triggered rules: ", triggeredRules, ms.Guild.ID)
	}
}

func (p *Plugin) RulesetRulesTriggered(ctxData *TriggeredRuleData) {
	ruleset := ctxData.Ruleset

	// check if we match all conditions, starting with the ruleset conditions
	if !p.CheckConditions(ctxData, ctxData.Ruleset.ParsedConditions) {
		return
	}

	filteredRules := make([]*ParsedRule, 0, len(ctxData.TriggeredRules))

	// Check the rule specific conditins
	for _, rule := range ctxData.TriggeredRules {
		ctxData.CurrentRule = rule

		if !p.CheckConditions(ctxData, rule.Conditions) {
			continue
		}

		// all conditions passed
		filteredRules = append(filteredRules, rule)
	}

	if len(filteredRules) < 1 {
		return // no rules passed
	}

	p.RulesetRulesTriggeredCondsPassed(ruleset, filteredRules, ctxData)

}

func (p *Plugin) CheckConditions(ctxData *TriggeredRuleData, conditions []*ParsedPart) bool {
	// check if we match all conditions, starting with the ruleset conditions
	for _, cond := range conditions {
		met, err := cond.Part.(Condition).IsMet(ctxData, cond.ParsedSettings)
		if err != nil {
			logrus.WithError(err).WithField("guild", ctxData.GS.ID).Error("failed checking if automod condition was met")
			return false // assume the condition failed
		}

		if !met {
			return false // condition was not met
		}
	}

	return true
}

func (p *Plugin) RulesetRulesTriggeredCondsPassed(ruleset *ParsedRuleset, triggeredRules []*ParsedRule, ctxData *TriggeredRuleData) {

	loggedModels := make([]*models.AutomodTriggeredRule, len(triggeredRules))

	// apply the effects
	for i, rule := range triggeredRules {
		ctxData.CurrentRule = rule

		for _, effect := range rule.Effects {
			go func(fx *ParsedPart, ctx *TriggeredRuleData) {
				err := fx.Part.(Effect).Apply(ctx, fx.ParsedSettings)
				if err != nil {
					logrus.WithError(err).WithField("guild", ruleset.RSModel.GuildID).WithField("part", fx.Part.Name()).Error("failed applying automod effect")
				}
			}(effect, ctxData.Clone())
		}

		// Log the rule activation
		cname := ""
		cid := int64(0)

		if ctxData.CS != nil {
			ctxData.CS.Owner.RLock()
			cname = ctxData.CS.Name
			ctxData.CS.Owner.RUnlock()
			cid = ctxData.CS.ID
		}

		tID := int64(0)
		tTypeID := 0
		for _, v := range ctxData.ActivatedTriggers {
			if v.RuleModel.RuleID == rule.Model.ID {
				tID = v.RuleModel.ID
				tTypeID = v.RuleModel.TypeID
				break
			}
		}

		serializedExtraData := []byte("{}")
		if ctxData.Message != nil {
			var err error
			serializedExtraData, err = json.Marshal(ctxData.Message)
			if err != nil {
				logrus.WithError(err).Error("automod failed serializing extra data")
				serializedExtraData = []byte("{}")
			}
		}

		loggedModels[i] = &models.AutomodTriggeredRule{
			ChannelID:     cid,
			ChannelName:   cname,
			GuildID:       ctxData.GS.ID,
			TriggerID:     null.Int64{Int64: tID, Valid: tID != 0},
			TriggerTypeid: tTypeID,
			RuleID:        null.Int64{Int64: rule.Model.ID, Valid: true},
			RuleName:      rule.Model.Name,
			RulesetName:   rule.Model.R.Ruleset.Name,
			UserID:        ctxData.MS.ID,
			UserName:      ctxData.MS.Username + "#" + ctxData.MS.StrDiscriminator(),
			Extradata:     serializedExtraData,
		}
	}

	tx, err := common.PQ.BeginTx(context.Background(), nil)
	if err != nil {
		logrus.WithError(err).Error("automod: failed creating transaction")
		return
	}

	for _, v := range loggedModels {
		err = v.Insert(context.Background(), tx, boil.Infer())
		if err != nil {
			logrus.WithError(err).Error("automod: failed inserting logged triggered rule")
			tx.Rollback()
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		logrus.WithError(err).Error("automod: failed committing logging transaction")
	}
}

type CacheKey int

const (
	CacheKeyRulesets CacheKey = iota
	CacheKeyLists
)

func (p *Plugin) FetchGuildRulesets(gs *dstate.GuildState) ([]*ParsedRuleset, error) {
	v, err := gs.UserCacheFetch(true, CacheKeyRulesets, func() (interface{}, error) {
		rulesets, err := models.AutomodRulesets(qm.Where("guild_id=?", gs.ID),
			qm.Load("RulesetAutomodRules.RuleAutomodRuleData"), qm.Load("RulesetAutomodRulesetConditions")).AllG(context.Background())

		if err != nil {
			return nil, err
		}

		parsedSets := make([]*ParsedRuleset, 0, len(rulesets))
		for _, v := range rulesets {
			parsed, err := ParseRuleset(v)
			if err != nil {
				return nil, err
			}
			parsedSets = append(parsedSets, parsed)
		}

		logrus.WithField("guild", gs.ID).WithField("n_rs", len(rulesets)).Info("fetched rulesets from db")

		return parsedSets, nil
	})

	if err != nil {
		return nil, err
	}

	cast := v.([]*ParsedRuleset)
	return cast, nil
}

func FetchGuildLists(gs *dstate.GuildState) ([]*models.AutomodList, error) {
	v, err := gs.UserCacheFetch(true, CacheKeyLists, func() (interface{}, error) {
		lists, err := models.AutomodLists(qm.Where("guild_id = ?", gs.ID)).AllG(context.Background())
		if err != nil {
			return nil, err
		}

		return []*models.AutomodList(lists), nil
	})

	if err != nil {
		return nil, err
	}

	cast := v.([]*models.AutomodList)
	return cast, nil
}

var ErrListNotFound = errors.New("list not found")

func FindFetchGuildList(gs *dstate.GuildState, listID int64) (*models.AutomodList, error) {
	lists, err := FetchGuildLists(gs)
	if err != nil {
		return nil, err
	}

	for _, v := range lists {
		if v.ID == listID {
			return v, nil
		}
	}

	return nil, ErrListNotFound
}