package monitor

import "errors"

var ErrInvalidInvestigationTransition = errors.New("invalid investigation transition")

func TransitionRun(run *Run, next RunStatus) error {
	if run == nil {
		return errors.New("monitor is required")
	}
	if err := validateRunTransition(run.Status, next); err != nil {
		return err
	}
	run.Status = next
	return nil
}

func TransitionInvestigation(investigation *Investigation, next string) error {
	if investigation == nil {
		return errors.New("investigation is required")
	}
	if investigation.Status == next {
		return nil
	}
	if investigation.Status == InvestigationWaiting {
		switch next {
		case InvestigationConfirmed, InvestigationSuppressed, InvestigationNeedsHuman, InvestigationFailed:
			investigation.Status = next
			return nil
		}
	}
	return ErrInvalidInvestigationTransition
}
