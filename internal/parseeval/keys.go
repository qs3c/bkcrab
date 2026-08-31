package parseeval

import (
	"errors"
	"fmt"
	"path"
	"regexp"
)

var parserEvalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validateObjectID(value string) error {
	if !parserEvalIDPattern.MatchString(value) {
		return errors.New("invalid parser evaluation identifier")
	}
	return nil
}

func RunObjectPrefix(runID string) (string, error) {
	if err := validateObjectID(runID); err != nil {
		return "", err
	}
	return path.Join("parser-eval", "runs", runID) + "/", nil
}

func DocumentObjectPrefix(runID, documentID string) (string, error) {
	if err := validateObjectID(runID); err != nil {
		return "", err
	}
	if err := validateObjectID(documentID); err != nil {
		return "", err
	}
	return path.Join("parser-eval", "runs", runID, "documents", documentID) + "/", nil
}

func SourceObjectKey(runID, documentID string) (string, error) {
	prefix, err := DocumentObjectPrefix(runID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "source.bin", nil
}

func TruthPageObjectKey(runID, documentID string, pageNumber int) (string, error) {
	if pageNumber <= 0 || pageNumber > 9999 {
		return "", errors.New("truth page number must be in [1,9999]")
	}
	prefix, err := DocumentObjectPrefix(runID, documentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%struth/page-%04d.png", prefix, pageNumber), nil
}

func MarkdownObjectKey(runID, documentID string, engine Engine) (string, error) {
	if !engine.Valid() {
		return "", errors.New("invalid parser engine")
	}
	prefix, err := DocumentObjectPrefix(runID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "outputs/" + string(engine) + ".md", nil
}

func JudgeRawObjectKey(runID, documentID string, order JudgeOrder) (string, error) {
	if !order.Valid() {
		return "", errors.New("invalid judge order")
	}
	prefix, err := DocumentObjectPrefix(runID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "judge/" + string(order) + ".json", nil
}
