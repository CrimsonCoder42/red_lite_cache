// Covers: backendAdvo cache layer
package conformance

// The key corpus: every distinct key SHAPE backendAdvo's cache/keys.go builds (not every builder —
// BPReadingKey and PulseReadingKey are one shape on two roots), each with a Sample that spells a key
// exactly as the cited builders do. The sweep tests (C4, C5, C6, C11) and Agent B's B1 benchmark
// draw their keys from here.
//
// after backendAdvo/cache/keys.go (read 2026-10-29, 5006 lines): pgtype.Timestamptz / pgtype.UUID /
// pgtype.Date are replaced by time.Time / uuid.UUID (zero = absent, rendered "0" / "none" as
// domainCursorKey and domainUUIDString do); tzDayRange is rendered in UTC.

import (
	"crypto/sha1"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Shape is one key shape of backendAdvo's cache layer.
type Shape struct {
	Name     string // "<domain>.<view>"
	Root     string // the key's first segment (the <x>KeyRoot const, or the literal head)
	Builders string // "keys.go:LINE FuncName, …" — where the spelling comes from
	// Sample builds one key of this shape. Profile-scoped shapes start with
	// DomainPatientPrefix(Root, profile); seed varies the parameters (window, cursor, limit, id).
	Sample        func(profile, tenant uuid.UUID, seed int) string
	ProfileScoped bool // true: reachable by InvalidateDomain(Root, profile)
}

// Domain is the part of Name before the first '.'.
func (s Shape) Domain() string { d, _, _ := strings.Cut(s.Name, "."); return d }

// CorpusKey is one generated key and the shape it came from.
type CorpusKey struct {
	Key   string
	Shape Shape
}

// ---- builder mirrors shared by the samples and the tests ----

// TZDayRange is "<loc>:<startOfLocalDay unix>:<nextLocalMidnight-1s unix>".
// after backendAdvo/cache/keys.go:4935-4939 (tzDayRange) + util/localtime.go:35-49: same spelling.
func TZDayRange(start, end time.Time) string {
	loc := start.Location()
	s := start.In(loc)
	sod := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
	e := end.In(loc)
	next := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return fmt.Sprintf("%s:%d:%d", loc.String(), sod.Unix(), next.Add(-time.Second).Unix())
}

// UUIDString renders an optional uuid segment: "none" when absent.
// after backendAdvo/cache/keys.go:69-74 (domainUUIDString).
func UUIDString(id uuid.UUID) string {
	if id == uuid.Nil {
		return "none"
	}
	return id.String()
}

// UserPrefix is "user:<uuid>". after backendAdvo/cache/keys.go:159-161.
func UserPrefix(userID uuid.UUID) string { return "user:" + userID.String() }

// UserPermissionsKey is "user:<uuid>:<tenant>:permissions".
// after backendAdvo/cache/keys.go:195-197 + :240 (userPermissionsSuffix).
func UserPermissionsKey(userID, tenantID uuid.UUID) string {
	return fmt.Sprintf("%s:%s%s", UserPrefix(userID), tenantID, UserPermissionsSuffix)
}

// UserPermissionsSuffix is the suffix backendAdvo's permission sweep removes.
// after backendAdvo/cache/keys.go:240; swept by cache/permissions.go:132 and cache/roles.go:93.
const UserPermissionsSuffix = ":permissions"

// UserProfileKey is "user:<uuid>:<tenant>:profile". after backendAdvo/cache/keys.go:187-189.
func UserProfileKey(userID, tenantID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:profile", UserPrefix(userID), tenantID)
}

// UserRecordKey is "user:<uuid>:<tenant>:record". after backendAdvo/cache/keys.go:176-178.
func UserRecordKey(userID, tenantID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:record", UserPrefix(userID), tenantID)
}

// ConditionListKey is "condition:patient:<p>:list:<cursor>:<limit>".
// after backendAdvo/cache/keys.go:1306-1308 (ConditionListKey).
func ConditionListKey(profile uuid.UUID, cursor time.Time, cursorID uuid.UUID, limit int) string {
	return fmt.Sprintf("%s:list:%s:%d", DomainPatientPrefix("condition", profile), DomainCursorKey(cursor, cursorID), limit)
}

// BPReadingsPageKey is "bp:patient:<p>:readings:<startNanos>:<endNanos>:<cursor>:<pageSize>".
// after backendAdvo/cache/keys.go:2834-2837 (vitalReadingsPageKey), :3054 (BPReadingsPageKey).
func BPReadingsPageKey(profile uuid.UUID, start, end, cursor time.Time, cursorID uuid.UUID, pageSize int) string {
	return fmt.Sprintf("%s:readings:%d:%d:%s:%d", DomainPatientPrefix("bp", profile),
		start.UnixNano(), end.UnixNano(), DomainCursorKey(cursor, cursorID), pageSize)
}

// ---- sample parameters, deterministic in (profile, seed) ----

var corpusBase = time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)

func sid(profile uuid.UUID, tag string, seed int) uuid.UUID {
	return uuid.NewSHA1(profile, fmt.Appendf(nil, "%s:%d", tag, seed))
}
func win(seed int) (time.Time, time.Time) {
	s := corpusBase.Add(time.Duration(seed) * 26 * time.Hour)
	return s, s.Add(7*24*time.Hour + 3*time.Hour)
}
func lim(seed int) int { return 10 * (1 + seed%3) }

// cur is page one ("0:none") on even seeds, a real (time, id) pair on odd ones.
func cur(profile uuid.UUID, seed int) string {
	if seed%2 == 0 {
		return DomainCursorKey(time.Time{}, uuid.Nil)
	}
	return DomainCursorKey(corpusBase.Add(time.Duration(seed)*time.Minute+123), sid(profile, "cursor", seed))
}

// optID is an optional uuid present on odd seeds.
func optID(profile uuid.UUID, tag string, seed int) string {
	if seed%2 == 0 {
		return "none"
	}
	return sid(profile, tag, seed).String()
}

// batchHash is "<n>:<first 8 bytes of sha1(sorted ids joined by sep)>".
// after backendAdvo/cache/keys.go:1633-1641 (MedDoseCountBatchKey), :4375-4383 (SleepSessOverlapsKey).
func batchHash(profile uuid.UUID, seed int, sep string) (int, string) {
	n := 1 + seed%3
	ids := make([]string, n)
	for i := range ids {
		ids[i] = sid(profile, "batch", seed*10+i).String()
	}
	sort.Strings(ids)
	sum := sha1.Sum([]byte(strings.Join(ids, sep)))
	return n, fmt.Sprintf("%x", sum[:8])
}

func pp(root string, profile uuid.UUID) string { return DomainPatientPrefix(root, profile) }

// ps is a profile-scoped shape.
func ps(name, root, builders string, f func(prefix string, p, t uuid.UUID, seed int) string) Shape {
	return Shape{Name: name, Root: root, Builders: builders, ProfileScoped: true,
		Sample: func(p, t uuid.UUID, seed int) string { return f(pp(root, p), p, t, seed) }}
}

// gl is a shape no profile sweep reaches (records by id, users, tenants, catalogs, blog, analytics).
func gl(name, root, builders string, f func(p, t uuid.UUID, seed int) string) Shape {
	return Shape{Name: name, Root: root, Builders: builders, Sample: f}
}

// Corpus is every key shape, grouped by domain.
var Corpus = []Shape{
	// ---- vitals: <root>:patient:<p>:<view>[:params], roots bp pulse bs o2 pf rr temp ----
	ps("vitals.reading", "bp", "keys.go:2767 vitalReadingKey, :3029 BPReadingKey, :3170 PulseReadingKey, :3347 BSReadingKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:reading:%s", x, sid(p, "reading", n))
		}),
	ps("vitals.summary", "pulse", "keys.go:2777 vitalSummaryKey, :3034 BPSummaryKey, :3175 PulseSummaryKey, :3352 BSSummaryKey",
		func(x string, _, _ uuid.UUID, n int) string { return fmt.Sprintf("%s:summary:%d:%d", x, 7+n, 30+n) }),
	ps("vitals.range", "bs", "keys.go:2785 vitalRangeKey, :3039 BPRangeKey, :3180 PulseRangeKey, :3357 BSRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:range:%s", x, TZDayRange(s, e))
		}),
	ps("vitals.range_analytics", "temp", "keys.go:2804 vitalAnalyticsKey, :111 domainAge, :3044 BPRangeAnalyticsKey, :3185 PulseRangeAnalyticsKey, :3362 BSRangeAnalyticsKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:range:%s:analytics:%.0f", x, TZDayRange(s, e), 39.6+float64(n))
		}),
	ps("vitals.o2_range_analytics", "o2", "keys.go:3683 O2RangeAnalyticsKey, :3753 o2ReportParams",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:range:%s:analytics:%.0f:%d", x, TZDayRange(s, e), 2.4+float64(n), n%3)
		}),
	ps("vitals.series", "bp", "keys.go:2818 vitalSeriesKey, :3049 BPSeriesKey, :3190 PulseSeriesKey, :3367 BSSeriesKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:series:%d:%s:%s", x, 3600*(1+n%4), "America/New_York", TZDayRange(s, e))
		}),
	ps("vitals.readings_page", "pulse", "keys.go:2834 vitalReadingsPageKey, :3054 BPReadingsPageKey, :3195 PulseReadingsPageKey, :3372 BSReadingsPageKey",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:readings:%d:%d:%s:%d", x, s.UnixNano(), e.UnixNano(), cur(p, n), lim(n))
		}),
	ps("vitals.feedday", "bs", "keys.go:2847 vitalFeedDayKey, :3059 BPFeedDayKey, :3200 PulseFeedDayKey, :3382 BSFeedDayKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:feedday:%s:%d:%d", x, "Europe/London", s.Unix(), e.Unix())
		}),
	ps("vitals.entity_range", "rr", "keys.go:2859 vitalEntityRangeKey, :3064 BPEntityRangeKey, :3205 PulseEntityRangeKey, :3387 BSEntityRangeKey",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			entity := []string{"patient", "episode", "condition"}[n%3]
			return fmt.Sprintf("%s:%s:%s:range:%s", x, entity, optID(p, "entity", n), TZDayRange(s, e))
		}),
	ps("vitals.window_count", "pf", "keys.go:2870 vitalWindowCountKey, :3090 BPWindowCountKey, :3220 PulseWindowCountKey, :3377 BSWindowCountKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:windowcount:%d:%d", x, s.UnixNano(), e.UnixNano())
		}),
	ps("vitals.aggregate", "bp", "keys.go:2883 vitalAggregateKey, :2890 PulseAggregateKey, :2895 BPAggregateKey, :2900 O2AggregateKey, :4848 RRAggregateKey",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:agg:%s:%s:%s:%s:%s", x, []string{"daily", "weekly"}[n%2], TZDayRange(s, e),
				optID(p, "ep", n), optID(p, "cond", n+1), fmt.Sprintf("p%d", n))
		}),
	ps("vitals.goal_active", "pulse", "keys.go:2911 vitalGoalActiveKey, :3215 PulseGoalActiveKey, :3397 BSGoalActiveKey",
		func(x string, _, _ uuid.UUID, _ int) string { return x + ":goal:active" }),
	ps("vitals.goal_measure_active", "bp", "keys.go:2923 vitalGoalActiveMeasureKey, :3074 BPGoalActiveKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:goal:%s:active", x, []string{"systolic", "diastolic"}[n%2])
		}),
	ps("vitals.context", "temp", "keys.go:2939 vitalContextKey, :2944 BPContextKey, :2974 TempContextKey",
		func(x string, _, _ uuid.UUID, n int) string { return fmt.Sprintf("%s:context:ctx%d", x, n) }),
	ps("vitals.hrbuckets", "pulse", "keys.go:3235 PulseHeartRateBucketsKey, :3245 PulseHeartRateDailyKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:hrbuckets:raw:%d:%d", x, s.UnixNano(), e.UnixNano())
		}),
	ps("vitals.analytics_result", "o2", "keys.go:4974 vitalAnalyticsResultKey, :4962 VitalAnalyticsParams.String, :4988 O2AnalyticsResultKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			cond := "-"
			if n%2 == 1 {
				cond = "c1+c2"
			}
			return fmt.Sprintf("%s:vanalytics:%s:%s:analytics:%s:%s:%s:%s:%s:%s", x, []string{"profile", "episode"}[n%2],
				TZDayRange(s, e), fmt.Sprintf("age:%d.5", 30+n), "rest", cond, "analytics", "v1", "family")
		}),

	// ---- infant care: diaper / feeding / sleep / sleepsess ----
	gl("infant.record", "diaper", "keys.go:3455 infantRecordKey, :3446 infantNorm, :3550 DiaperRecordKey, :4253 SleepRecordKey, :4443 FeedingRecordKey",
		func(p, _ uuid.UUID, n int) string {
			k := fmt.Sprintf("%s:record:%s", "diaper", sid(p, "infant", n))
			if n%2 == 1 {
				k += ":norm"
			}
			return k
		}),
	ps("infant.list", "diaper", "keys.go:3460 infantListKey, :3555 DiaperListKey, :4258 SleepListKey, :4448 FeedingListKey, :4336 SleepSessListKey",
		func(x string, p, _ uuid.UUID, n int) string {
			k := fmt.Sprintf("%s:list:%s:%d", x, cur(p, n), lim(n))
			if n%2 == 0 {
				k += ":norm"
			}
			return k
		}),
	ps("infant.count", "feeding", "keys.go:3466 infantCountKey, :3560 DiaperCountKey, :4263 SleepCountKey, :4453 FeedingCountKey",
		func(x string, _, _ uuid.UUID, _ int) string { return x + ":count" }),
	ps("infant.buckets", "sleep", "keys.go:3481 infantBucketsKey, :3575 DiaperBucketsKey, :4300 SleepBucketsKey, :4468 FeedingBucketsKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:buckets:%s:%d:%d", x, "America/Chicago", s.Unix(), e.Unix())
		}),
	ps("infant.subtypes", "feeding", "keys.go:3492 infantSubtypeSummaryKey, :3580 DiaperSubtypeSummaryKey, :4473 FeedingSubtypeSummaryKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:subtypes:%s:%d:%d", x, "UTC", 7, 30+n)
		}),
	ps("infant.range", "diaper", "keys.go:3500 infantRangeKey, :3570 DiaperRangeKey, :4295 SleepRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			k := fmt.Sprintf("%s:range:%s", x, TZDayRange(s, e))
			if n%2 == 1 {
				k += ":norm"
			}
			return k
		}),
	ps("infant.range_limit", "feeding", "keys.go:3511 infantRangeLimitKey, :4463 FeedingRangeKey, :4356 SleepSessRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:range:%s:%d", x, TZDayRange(s, e), lim(n))
		}),
	ps("infant.sleep_active", "sleep", "keys.go:4290 SleepActiveKey, :4347 SleepSessActiveKey",
		func(x string, _, _ uuid.UUID, n int) string {
			if n%2 == 1 {
				return x + ":active:norm"
			}
			return x + ":active"
		}),
	ps("infant.sleepsess_score", "sleepsess", "keys.go:4402 SleepSessScoreKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:sleepscore:%s:%s:m%d", x, "UTC", corpusBase.AddDate(0, 0, n).Format("2006-01-02"), 3+n)
		}),
	ps("infant.sleepsess_overlaps", "sleepsess", "keys.go:4375 SleepSessOverlapsKey",
		func(x string, p, _ uuid.UUID, n int) string {
			c, h := batchHash(p, n, ",")
			return fmt.Sprintf("%s:overlaps:%d:%s", x, c, h)
		}),

	// ---- medical: conditions / episodes / allergies / medications / immunizations / symptoms ----
	ps("medical.condition_record", "condition", "keys.go:1300 ConditionRecordKey, :1260 EpisodeRecordKey, :894 AllergyRecordKey, :4593 SymptomRecordKey",
		func(x string, p, _ uuid.UUID, n int) string { return fmt.Sprintf("%s:record:%s", x, sid(p, "cond", n)) }),
	ps("medical.condition_list", "condition", "keys.go:1306 ConditionListKey, :1266 EpisodeListKey, :900 AllergyListKey, :1224 FamilyListKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:list:%s:%d", x, cur(p, n), lim(n))
		}),
	ps("medical.condition_notes", "condition", "keys.go:1341 ConditionNotesKey, :1352 EpisodeNotesKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:notes:%s:%s:%d", x, sid(p, "cond", n), cur(p, n), lim(n))
		}),
	ps("medical.condition_feed", "condition", "keys.go:127 domainFeedWindowKey, :1328 ConditionFeedWindowKey, :1290 EpisodeFeedWindowKey, :916 AllergyFeedWindowKey",
		func(x string, _, _ uuid.UUID, n int) string { s, e := win(n); return DomainFeedWindowKey(x, s, e) }),
	ps("medical.episode_notescount", "episode", "keys.go:1357 EpisodeNotesCountKey, :1346 ConditionNotesCountKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:notescount:%s", x, sid(p, "ep", n))
		}),
	ps("medical.episode_stats", "episode", "keys.go:1276 EpisodeStatsKey, :1271 EpisodeCountKey, :1316 ConditionStatsKey, :910 AllergyStatsKey",
		func(x string, _, _ uuid.UUID, n int) string { return x + []string{":stats", ":count"}[n%2] }),
	ps("medical.allergy_food_names", "allergy", "keys.go:2501 AllergyFoodNamesKey",
		func(x string, _, _ uuid.UUID, _ int) string { return x + ":food_names" }),
	ps("medical.med_record", "med", "keys.go:1537 MedRecordKey, :1544 MedRecordWithLastTakenKey",
		func(x string, p, _ uuid.UUID, n int) string {
			k := fmt.Sprintf("%s:record:%s:%s", x, []string{"any", "scheduled", "prn"}[n%3], sid(p, "med", n))
			if n%2 == 1 {
				k += ":withlasttaken"
			}
			return k
		}),
	ps("medical.med_list", "med", "keys.go:1568 MedListKey, :1576 MedListByTypeKey",
		func(x string, p, _ uuid.UUID, n int) string {
			if n%2 == 1 {
				return fmt.Sprintf("%s:list:type:%d:%s:%d", x, n%2, cur(p, n), lim(n))
			}
			return fmt.Sprintf("%s:list:%s:%s:%d", x, "scheduled", cur(p, n), lim(n))
		}),
	ps("medical.med_dose_offset_range", "med", "keys.go:1604 MedDoseOffsetRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:doses:range:%s:%d:%d", x, TZDayRange(s, e), lim(n), 20*n)
		}),
	ps("medical.med_dose_count_batch", "med", "keys.go:1633 MedDoseCountBatchKey",
		func(x string, p, _ uuid.UUID, n int) string {
			c, h := batchHash(p, n, ",")
			return fmt.Sprintf("%s:doses:count:batch:%d:%s", x, c, h)
		}),
	ps("medical.med_adherence", "med", "keys.go:1685 MedAdherenceKey",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:adherence:%s:%s", x, sid(p, "med", n), TZDayRange(s, e))
		}),
	ps("medical.med_count_window", "med", "keys.go:1738 MedCountWindowKey, :1745 medWindowBound",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			lo := fmt.Sprintf("%d", s.Unix())
			if n%2 == 0 {
				lo = "db"
			}
			return fmt.Sprintf("%s:count:%s:%s:%d", x, "active", lo, e.Unix())
		}),
	ps("medical.med_timeline", "med", "keys.go:1696 MedTimelineKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:timeline:%d:%d:%d", x, s.UnixNano(), e.UnixNano(), lim(n))
		}),
	ps("medical.immunization_range", "immunization", "keys.go:1006 ImmunizationRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:range:%d:%d:%d:%d", x, s.UnixNano(), e.UnixNano(), lim(n), 10*n)
		}),
	ps("medical.immunization_stats", "immunization", "keys.go:996 ImmunizationStatsKey, :1384 ImmunizationUpcomingOnKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:stats:%s", x, corpusBase.AddDate(0, 0, n).Format("2006-01-02"))
		}),
	ps("medical.symptom_feed_seconds", "symptoms", "keys.go:4752 SymptomFeedWindowKey (Unix SECONDS, not domainFeedWindowKey)",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:feed:%d:%d", x, s.Unix(), e.Unix())
		}),
	ps("medical.symptom_groups", "symptoms", "keys.go:4634 SymptomGroupsKey, :4568 symptomScopeKey",
		func(x string, p, _ uuid.UUID, n int) string {
			scope := []string{"", ":ep:" + sid(p, "ep", n).String(), ":cond:" + sid(p, "cond", n).String()}[n%3]
			return fmt.Sprintf("%s:groups%s:%s", x, scope, "UTC")
		}),
	ps("medical.econtact_list", "econtact", "keys.go:614 EContactListKey, :619 EContactRecordKey",
		func(x string, p, _ uuid.UUID, n int) string {
			if n%2 == 1 {
				return fmt.Sprintf("%s:record:%s", x, sid(p, "econtact", n))
			}
			return x + ":list"
		}),
	ps("medical.assoc_caregivers", "assoc", "keys.go:460 AssocCaregiverListKey, :408 AssocProfilePrefix, :438 AssocRowKey",
		func(x string, p, _ uuid.UUID, n int) string {
			if n%2 == 1 {
				return fmt.Sprintf("%s:user:%s:row", x, sid(p, "user", n))
			}
			return x + ":caregivers"
		}),

	// ---- fitness ----
	ps("fitness.routine_list", "fitroutine", "keys.go:1954 FitnessRoutineListKey, :1965 FitnessRoutineWholeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:list:%s", x, []string{"active", "all"}[n%2])
		}),
	ps("fitness.plan_today", "fitplan", "keys.go:2042 FitnessPlanTodayKey, :2032 FitnessPlanEntriesAllKey",
		func(x string, _, _ uuid.UUID, n int) string { return fmt.Sprintf("%s:today:%d", x, n%7) }),
	ps("fitness.log_list", "fitlog", "keys.go:2120 FitnessLogListKey, :2237 FitnessActivityListKey",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:list:%s:%d:%d:%s:%d", x, "strength", s.Unix(), e.Unix(), cur(p, n), lim(n))
		}),
	ps("fitness.log_buckets", "fitlog", "keys.go:2166 FitnessLogBucketsKey, :2220 FitnessLogFeedDayKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:buckets:v3:%s:%s:%d:%d", x, []string{"day", "week"}[n%2], "UTC", s.Unix(), e.Unix())
		}),
	ps("fitness.activity_daily", "fitactivity", "keys.go:2257 FitnessActivityDailyKey, :2265 FitnessActivityStatsKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:daily:v2:%s:%s:%d:%d", x, []string{"walking", "running"}[n%2], "UTC", s.Unix(), e.Unix())
		}),
	gl("fitness.catalog_list", "fitcat", "keys.go:1848 FitnessCatalogListKey, :1869 FitnessExerciseTypeListKey",
		func(_, _ uuid.UUID, n int) string {
			return fmt.Sprintf("fitcat:list:%s:%s:%d:%d", "plan", "all", lim(n), 20*n)
		}),

	// ---- nutrition ----
	ps("nutrition.day", "nutrition", "keys.go:2338 NutritionDayKey, :2382 NutritionDayMealsKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:day:%s", x, corpusBase.AddDate(0, 0, n).Format("2006-01-02"))
		}),
	ps("nutrition.range", "nutrition", "keys.go:2346 NutritionRangeKey, :2325 nutritionDate",
		func(x string, p, _ uuid.UUID, n int) string {
			s, e := win(n)
			cd := "none"
			if n%2 == 1 {
				cd = s.AddDate(0, 0, 1).Format("2006-01-02")
			}
			return fmt.Sprintf("%s:range:%s:%s:%s:%s:%d", x, s.Format("2006-01-02"), e.Format("2006-01-02"), cd, optID(p, "nut", n), lim(n))
		}),
	ps("nutrition.recent_foods", "nutrition", "keys.go:2485 NutritionRecentFoodsKey, :2464 nutritionMentionsKey, :2475 nutritionMealKey",
		func(x string, p, _ uuid.UUID, n int) string {
			meal := []string{"all", "breakfast"}[n%2]
			mentions := "none"
			if n%2 == 1 {
				terms := []string{"oat", "apple"}
				sort.Strings(terms)
				sum := sha1.Sum([]byte(strings.Join(terms, "\x1f")))
				mentions = fmt.Sprintf("%d-%x", len(terms), sum[:8])
			}
			return fmt.Sprintf("%s:recent_foods:%s:%d:%s", x, meal, lim(n), mentions)
		}),
	gl("nutrition.food_record", "nutritionfood", "keys.go:2458 NutritionFoodRecordKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("nutritionfood:record:%s", sid(p, "food", n)) }),

	// ---- measurements ----
	ps("measurements.list_analytics", "measurement", "keys.go:1170 MeasurementListAnalyticsKey, :1159 MeasurementListKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:list:%s:%d:analytics:%.2f", x, cur(p, n), lim(n), 1.25+float64(n))
		}),
	ps("measurements.goal_history", "measurement", "keys.go:1195 MeasurementGoalHistoryKey, :1190 MeasurementGoalKey",
		func(x string, _, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:goal_history:%s:%d", x, []string{"weight", "height"}[n%2], lim(n))
		}),
	ps("measurements.weight_aggregate", "measurement", "keys.go:4887 WeightAggregateKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:agg:weight:%s:%s", x, TZDayRange(s, e), fmt.Sprintf("p%d", n))
		}),
	ps("measurements.feed", "measurement", "keys.go:1200 MeasurementFeedWindowKey, :127 domainFeedWindowKey",
		func(x string, _, _ uuid.UUID, n int) string { s, e := win(n); return DomainFeedWindowKey(x, s, e) }),

	// ---- events ----
	ps("events.list", "events", "keys.go:788 EventListKey",
		func(x string, p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("%s:list:%s:%d", x, cur(p, n), lim(n))
		}),
	ps("events.daterange", "events", "keys.go:815 EventDateRangeKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, e := win(n)
			return fmt.Sprintf("%s:daterange:%d:%d", x, s.Unix(), e.Unix())
		}),
	ps("events.upcoming", "events", "keys.go:827 EventUpcomingKey",
		func(x string, _, _ uuid.UUID, n int) string {
			s, _ := win(n)
			return fmt.Sprintf("%s:upcoming:%d:%d", x, s.Unix(), lim(n))
		}),
	gl("events.tenant_record", "events", "keys.go:778 EventRecordKey, :770 EventTenantPrefix",
		func(p, t uuid.UUID, n int) string {
			return fmt.Sprintf("events:tenant:%s:record:%s", t, sid(p, "event", n))
		}),

	// ---- users / permissions / tenants ----
	gl("users.record", "user", "keys.go:176 UserRecordKey, :182 UserTenantMemberKey",
		func(p, t uuid.UUID, n int) string { return UserRecordKey(sid(p, "user", n), t) }),
	gl("users.profile", "user", "keys.go:187 UserProfileKey",
		func(p, t uuid.UUID, n int) string { return UserProfileKey(sid(p, "user", n), t) }),
	gl("users.permissions", "user", "keys.go:195 UserPermissionsKey, :240 userPermissionsSuffix",
		func(p, t uuid.UUID, n int) string { return UserPermissionsKey(sid(p, "user", n), t) }),
	gl("users.roles", "user", "keys.go:214 UserRolesKey, :204 UserRoleIDsKey",
		func(p, t uuid.UUID, n int) string {
			return fmt.Sprintf("%s:%s%s", UserPrefix(sid(p, "user", n)), t, []string{":roles", ":role_ids"}[n%2])
		}),
	gl("users.tenants", "user", "keys.go:219 UserTenantsKey, :224 UserTenantKey",
		func(p, _ uuid.UUID, n int) string {
			return UserPrefix(sid(p, "user", n)) + []string{":tenants", ":tenant"}[n%2]
		}),
	gl("users.by_email", "user", "keys.go:259 UserByEmailKey, :266 UserByAppleIDKey, :270 UserByGoogleIDKey",
		func(p, _ uuid.UUID, n int) string {
			return fmt.Sprintf("user:%s:%s", []string{"email", "apple", "google"}[n%3], sid(p, "alias", n).String()[:8]+"@example.test")
		}),
	gl("users.notif_unread", "user", "keys.go:4906 NotifUnreadCountKey, :4911 NotifRecentKey, :4898 NotifUserTenantPrefix",
		func(p, t uuid.UUID, n int) string {
			return fmt.Sprintf("%s:notif:%s%s", UserPrefix(sid(p, "user", n)), t, []string{":unread_count", ":recent"}[n%2])
		}),
	gl("users.tenant_users", "t", "keys.go:279 TenantPrefixedKey, :292 TenantUsersKey, :302 TenantUsersExcludingKey",
		func(p, t uuid.UUID, n int) string {
			if n%2 == 1 {
				return fmt.Sprintf("t:%s:users:exclude:%s", t, sid(p, "user", n))
			}
			return fmt.Sprintf("t:%s:users", t)
		}),
	gl("users.role_holders", "role", "keys.go:341 RoleHoldersKey, :329 RoleAvailableKey",
		func(_, t uuid.UUID, n int) string {
			return fmt.Sprintf("role:users:%s:%s", fmt.Sprintf("role%d", n), t)
		}),
	gl("users.admin_tenant_summary", "admin", "keys.go:4838 AdminTenantSummaryKey, :4832 AdminTenantByIDKey",
		func(_, t uuid.UUID, n int) string {
			if n%2 == 1 {
				return fmt.Sprintf("admin:tenant:id:%s", t)
			}
			return fmt.Sprintf("admin:tenant:%s:summary", t)
		}),
	gl("users.bloodtype_distribution", "bloodtype", "keys.go:955 BloodTypeDistributionKey",
		func(_, t uuid.UUID, _ int) string { return fmt.Sprintf("bloodtype:tenant:%s:distribution", t) }),

	// ---- analytics (global) ----
	gl("analytics.newsletter", "newsletter", "keys.go:1759 NewsletterAnalyticsKey",
		func(_, _ uuid.UUID, _ int) string { return "newsletter:analytics:stats" }),
	gl("analytics.site", "site", "keys.go:1768 SiteAnalyticsKey",
		func(_, _ uuid.UUID, _ int) string { return "site:analytics" }),
	gl("analytics.blog_performance", "blog", "keys.go:1409 BlogAnalyticsKey",
		func(_, _ uuid.UUID, n int) string {
			return fmt.Sprintf("blog:analytics:performance:%s", []string{"7d", "30d"}[n%2])
		}),

	// ---- blog / campaign ----
	gl("blog.post", "blog", "keys.go:1399 BlogKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("blog:%s", sid(p, "blog", n)) }),
	gl("blog.list", "blog", "keys.go:1404 BlogListKey",
		func(_, _ uuid.UUID, n int) string {
			return fmt.Sprintf("blog:list:%s", []string{"published", "draft"}[n%2])
		}),
	gl("campaign.record", "image_campaign", "keys.go:1418 ImageCampaignKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("image_campaign:%s", sid(p, "campaign", n)) }),
	gl("campaign.list", "image_campaigns", "keys.go:1423 ImageCampaignListKey",
		func(_, _ uuid.UUID, n int) string {
			return fmt.Sprintf("image_campaigns:list:%s", []string{"active", "done"}[n%2])
		}),

	// ---- test results ----
	ps("testresults.record", "tr", "keys.go:1059 TestResultProfileRecordKey",
		func(x string, p, _ uuid.UUID, n int) string { return fmt.Sprintf("%s:record:%s", x, sid(p, "tr", n)) }),
	ps("testresults.list_date_cursor", "tr", "keys.go:1065 TestResultListKey, :1124 testResultCursorKey",
		func(x string, p, _ uuid.UUID, n int) string {
			c := "none:none"
			if n%2 == 1 {
				c = corpusBase.AddDate(0, 0, -n).Format("2006-01-02") + ":" + sid(p, "trc", n).String()
			}
			return fmt.Sprintf("%s:list:%s:%d", x, c, lim(n))
		}),
	ps("testresults.entity_list", "tr", "keys.go:1107 TestResultEntityListKey, :1113 TestResultEntityCountKey",
		func(x string, p, t uuid.UUID, n int) string {
			return fmt.Sprintf("%s:entity:%s:%s:%s:%s:%d", x, []string{"episode", "condition"}[n%2], optID(p, "ent", n+1), t, "none:none", lim(n))
		}),
	ps("testresults.feed", "tr", "keys.go:1085 TestResultFeedWindowKey, :127 domainFeedWindowKey",
		func(x string, _, _ uuid.UUID, n int) string { s, e := win(n); return DomainFeedWindowKey(x, s, e) }),

	// ---- records: profile-independent <entity>:record:<id> and catalogs ----
	gl("records.patient_tenant", "patient", "keys.go:549 PatientRecordKey, :544 PatientTenantPrefix",
		func(p, t uuid.UUID, n int) string {
			return fmt.Sprintf("patient:tenant:%s:record:%s", t, sid(p, "patient", n))
		}),
	gl("records.patient_sub", "patient", "keys.go:573 PatientSubKey",
		func(p, _ uuid.UUID, n int) string {
			// the real profile id: "patient:<p>:…" must NOT be reached by "<root>:patient:<p>"
			return fmt.Sprintf("patient:%s:%s", p, []string{"dob", "summary"}[n%2])
		}),
	gl("records.feeding", "feeding", "keys.go:4443 FeedingRecordKey, :3455 infantRecordKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("feeding:record:%s", sid(p, "feedrec", n)) }),
	gl("records.sleepsess", "sleepsess", "keys.go:4331 SleepSessRecordKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("sleepsess:record:%s", sid(p, "ssrec", n)) }),
	gl("records.invite", "invite", "keys.go:519 CaregiverInviteRecordKey, :366 InviteRecordKey",
		func(p, _ uuid.UUID, n int) string { return fmt.Sprintf("invite:id:%s", sid(p, "invite", n)) }),
	gl("records.immunization_catalog", "immunization", "keys.go:1029 ImmunizationCatalogRecordKey, :1024 ImmunizationCatalogListKey",
		func(_, _ uuid.UUID, n int) string {
			return fmt.Sprintf("immunization:catalog:record:%s", fmt.Sprintf("vac%d", n))
		}),
	gl("records.symptom_catalog", "symptomref", "keys.go:4787 SymptomCatalogEntryKey, :4797 SymptomCatalogListKey",
		func(_, _ uuid.UUID, n int) string { return fmt.Sprintf("symptomref:entry:%d", 100+n) }),
}

// Roots returns every distinct root of a profile-scoped shape, sorted: the roots InvalidateDomain
// is called with.
func Roots() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range Corpus {
		if s.ProfileScoped && !seen[s.Root] {
			seen[s.Root] = true
			out = append(out, s.Root)
		}
	}
	sort.Strings(out)
	return out
}

// KeysFor returns n samples (seeds 0..n-1) of every shape for profile and tenant, duplicates
// dropped (a shape with no varying parameter yields one key).
func KeysFor(profile, tenant uuid.UUID, n int) []CorpusKey {
	seen := map[string]bool{}
	var out []CorpusKey
	for _, s := range Corpus {
		for seed := range n {
			k := s.Sample(profile, tenant, seed)
			if !seen[k] {
				seen[k] = true
				out = append(out, CorpusKey{Key: k, Shape: s})
			}
		}
	}
	return out
}
