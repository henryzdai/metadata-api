package metadata

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"
)

func ValidUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' || value[14] != '4' ||
		!strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func ParseMaintainerID(value string) (MaintainerID, error) {
	if !ValidUUID(value) {
		return "", NewError(InvalidID, nil)
	}
	return MaintainerID(value), nil
}

func ParseApplicationID(value string) (ApplicationID, error) {
	if !ValidUUID(value) {
		return "", NewError(InvalidID, nil)
	}
	return ApplicationID(value), nil
}

func NormalizeMaintainer(fields MaintainerFields) (MaintainerFields, error) {
	fields.Name = strings.TrimSpace(fields.Name)
	fields.Email = strings.TrimSpace(fields.Email)
	invalid := make(map[string]string)
	checkString(invalid, "name", fields.Name, 128, true)
	checkString(invalid, "email", fields.Email, 254, true)
	if fields.Email != "" {
		plainASCII := true
		for _, char := range fields.Email {
			if char < 33 || char > 126 {
				plainASCII = false
				break
			}
		}
		address, err := mail.ParseAddress(fields.Email)
		if !plainASCII || err != nil || address.Address != fields.Email {
			invalid["email"] = "Must be a plain ASCII email address."
		}
	}
	if len(invalid) != 0 {
		return MaintainerFields{}, NewError(ValidationFailed, invalid)
	}
	// Validate the original alphabet before case folding can turn Unicode into ASCII.
	fields.Email = strings.ToLower(fields.Email)
	return fields, nil
}

func NormalizeApplication(fields ApplicationFields) (ApplicationFields, error) {
	fields.Title = strings.TrimSpace(fields.Title)
	fields.Version = strings.TrimSpace(fields.Version)
	fields.Company = strings.TrimSpace(fields.Company)
	fields.Website = strings.TrimSpace(fields.Website)
	fields.Source = strings.TrimSpace(fields.Source)
	fields.License = strings.TrimSpace(fields.License)
	fields.Description = strings.TrimSpace(fields.Description)
	invalid := make(map[string]string)
	checkString(invalid, "title", fields.Title, 256, true)
	checkString(invalid, "version", fields.Version, 64, true)
	checkString(invalid, "company", fields.Company, 256, true)
	checkString(invalid, "website", fields.Website, 2048, false)
	checkString(invalid, "source", fields.Source, 2048, false)
	checkString(invalid, "license", fields.License, 128, false)
	checkString(invalid, "description", fields.Description, 16384, false)
	for name, value := range map[string]string{"website": fields.Website, "source": fields.Source} {
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
			!(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
			invalid[name] = "Must be an absolute HTTP(S) URL with a host and no userinfo."
		}
	}
	if len(invalid) != 0 {
		return ApplicationFields{}, NewError(ValidationFailed, invalid)
	}
	return fields, nil
}

func NormalizeCreateApplication(input CreateApplicationInput) (CreateApplicationInput, error) {
	fields, err := NormalizeApplication(input.ApplicationFields)
	if err != nil {
		return CreateApplicationInput{}, err
	}
	input.ApplicationFields = fields
	input.MaintainerID = MaintainerID(strings.TrimSpace(string(input.MaintainerID)))
	if !ValidUUID(string(input.MaintainerID)) {
		return CreateApplicationInput{}, NewError(ValidationFailed, map[string]string{
			"maintainer_id": "Must be a canonical lowercase UUIDv4.",
		})
	}
	return input, nil
}

func checkString(invalid map[string]string, name, value string, limit int, required bool) {
	switch {
	case !utf8.ValidString(value):
		invalid[name] = "Must be valid UTF-8."
	case required && value == "":
		invalid[name] = "Is required."
	case len(value) > limit:
		invalid[name] = "Exceeds the byte limit."
	}
}
