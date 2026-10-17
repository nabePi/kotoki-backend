package scenario

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// revertTimeout bounds the revert and the stop of the load on the way out: they run on a context
// that survives the cancellation of the run, because a fault that is left in place is worse than
// a run that took a minute longer to end.
const revertTimeout = 2 * time.Minute

// Run plays sc: it starts the load, injects at InjectAt, reverts at RevertAt, keeps the load going
// until Total, stops it, runs kotoki-verify and evaluates the assertions (spec 13.13.1). The revert
// and the stop of the load run on every path out, including a failed injection and a cancelled ctx.
//
// A run that could not be carried out returns an error (and a Report with Pass false); a run that
// was carried out and whose assertions failed returns the Report with Pass false and no error.
func Run(ctx context.Context, sc Scenario, d Deps, o Options) (rep Report, err error) {
	rep = Report{ID: sc.ID}
	if err = o.validate(); err != nil {
		return rep, err
	}
	if o.Env == Compose && !sc.LocalOK {
		return rep, fmt.Errorf("%s: %w", sc.ID, ErrNeedsCluster)
	}
	scale := o.Scale
	if scale == 0 {
		scale = 1
	}
	if sc.Fixed != nil { // real durations: the experiment's clock is not compressed
		o.InjectAt, o.RevertAt, o.Total, scale = sc.Fixed.InjectAt, sc.Fixed.RevertAt, sc.Fixed.RevertAt+sc.Fixed.Tail, 1
	}
	wait := func(d time.Duration) time.Duration { return time.Duration(math.Round(float64(d) * scale)) }

	if d.Prepare != nil {
		if err = d.Prepare(ctx); err != nil {
			return rep, fmt.Errorf("%s: prepare: %w", sc.ID, err)
		}
	}
	stop, err := d.Load.Start(ctx, o.VUs)
	if err != nil {
		return rep, fmt.Errorf("%s: start load: %w", sc.ID, err)
	}
	since := d.Clock.Now()
	stopped := false
	stopLoad := func() error {
		if stopped {
			return nil
		}
		stopped = true
		return stop()
	}
	reverted := false
	revert := func() error {
		if reverted {
			return nil
		}
		reverted = true
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revertTimeout)
		defer cancel()
		var errs []error
		for _, a := range sc.Revert[o.Env] {
			if e := d.Driver.Do(rctx, a); e != nil {
				errs = append(errs, fmt.Errorf("revert: %w", e))
			}
		}
		return errors.Join(errs...)
	}
	defer func() {
		// the way out of a failed run: undo the fault, stop the load
		if e := revert(); e != nil {
			err = errors.Join(err, e)
		}
		if e := stopLoad(); e != nil {
			err = errors.Join(err, fmt.Errorf("stop load: %w", e))
		}
		if err != nil {
			rep.Pass = false
			if rep.Reason == "" {
				rep.Reason = err.Error()
			}
		}
	}()

	if err = d.Clock.Sleep(ctx, wait(o.InjectAt)); err != nil {
		return rep, fmt.Errorf("%s: %w", sc.ID, err)
	}
	for _, a := range sc.Inject[o.Env] {
		if err = d.Driver.Do(ctx, a); err != nil {
			return rep, fmt.Errorf("%s: inject: %w", sc.ID, err)
		}
	}
	if err = d.Clock.Sleep(ctx, wait(o.RevertAt-o.InjectAt)); err != nil {
		return rep, fmt.Errorf("%s: %w", sc.ID, err)
	}
	if err = revert(); err != nil {
		return rep, fmt.Errorf("%s: %w", sc.ID, err)
	}
	// the recovery after the revert is not compressed by Scale: a breaker waits 20 s and a pod
	// restarts in the time it takes, whatever the experiment's clock
	if tail := o.Total - o.RevertAt; tail > 0 {
		if err = d.Clock.Sleep(ctx, tail); err != nil {
			return rep, fmt.Errorf("%s: %w", sc.ID, err)
		}
	}
	if err = stopLoad(); err != nil {
		return rep, fmt.Errorf("%s: stop load: %w", sc.ID, err)
	}

	violations, err := d.Verify.Run(ctx, since)
	if err != nil {
		return rep, fmt.Errorf("%s: verify: %w", sc.ID, err)
	}
	rep = evaluate(ctx, sc, d, o.Env, violations)
	return rep, nil
}

func (o Options) validate() error {
	switch {
	case o.Env != Compose && o.Env != Kube:
		return fmt.Errorf("scenario: unknown environment %q", o.Env)
	case o.VUs <= 0:
		return errors.New("scenario: VUs must be positive")
	case o.Scale < 0:
		return errors.New("scenario: scale cannot be negative")
	case o.InjectAt < 0 || o.RevertAt <= o.InjectAt:
		return errors.New("scenario: RevertAt must be after InjectAt")
	}
	return nil
}

// evaluate checks the assertions that apply to env, after the load stopped.
func evaluate(ctx context.Context, sc Scenario, d Deps, env Env, violations int) Report {
	rep := Report{ID: sc.ID, Pass: true}
	var failed []string
	fail := func(name string) { rep.Pass = false; failed = append(failed, name) }
	sawVerify, manual := false, false
	for _, a := range sc.Expect {
		if a.Only != "" && a.Only != env {
			continue
		}
		r := AssertionResult{Name: a.Name, Kind: a.Kind}
		switch a.Kind {
		case AssertProm:
			v, err := d.Prom.Query(ctx, a.Expr)
			r.Value = v
			switch {
			case err != nil:
				r.Detail = "query failed: " + err.Error()
			case !compare(v, a.Cmp, a.Want):
				r.Detail = fmt.Sprintf("%g %s %g is false (%s)", v, a.Cmp, a.Want, a.Expr)
			default:
				r.Pass = true
			}
		case AssertVerify:
			sawVerify = true
			r.Value = float64(violations)
			if compare(r.Value, "==", a.Want) {
				r.Pass = true
			} else {
				r.Detail = fmt.Sprintf("kotoki-verify reported %d violation(s), want %g", violations, a.Want)
			}
		case AssertManual:
			manual = true
			r.Detail = "manual check required"
		default:
			r.Detail = fmt.Sprintf("unknown assertion kind %q", a.Kind)
		}
		if !r.Pass {
			fail(a.Name)
		}
		rep.Results = append(rep.Results, r)
	}
	if !sawVerify && violations != 0 {
		// kotoki-verify belongs to every scenario (13.13.1) whether or not the table lists it
		rep.Results = append(rep.Results, AssertionResult{Name: "kotoki-verify", Kind: AssertVerify,
			Value: float64(violations), Detail: fmt.Sprintf("kotoki-verify reported %d violation(s)", violations)})
		fail("kotoki-verify")
	}
	switch {
	case rep.Pass:
	case manual && len(failed) == countManual(rep.Results):
		rep.Reason = "manual check required"
	default:
		rep.Reason = "failed: " + strings.Join(failed, "; ")
	}
	return rep
}

func countManual(rs []AssertionResult) int {
	n := 0
	for _, r := range rs {
		if r.Kind == AssertManual {
			n++
		}
	}
	return n
}

func compare(v float64, cmp string, want float64) bool {
	switch cmp {
	case ">":
		return v > want
	case ">=":
		return v >= want
	case "<":
		return v < want
	case "<=":
		return v <= want
	case "==":
		return v == want
	case "!=":
		return v != want
	}
	return false
}
