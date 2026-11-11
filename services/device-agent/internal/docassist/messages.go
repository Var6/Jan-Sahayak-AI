package docassist

// Citizen-facing wording. None of it is a government fact: document type
// names are generic, and anything scheme-specific comes from the knowledge
// record under the display policy.

var typeLabels = map[DocType][2]string{ // {hi, en}
	Aadhaar:              {"आधार कार्ड", "Aadhaar card"},
	RationCard:           {"राशन कार्ड", "Ration card"},
	CasteCertificate:     {"जाति प्रमाण पत्र", "Caste certificate"},
	IncomeCertificate:    {"आय प्रमाण पत्र", "Income certificate"},
	ResidenceCertificate: {"निवास प्रमाण पत्र", "Residence certificate"},
	BankPassbook:         {"बैंक पासबुक", "Bank passbook"},
	VoterID:              {"वोटर पहचान पत्र", "Voter ID card"},
	PAN:                  {"पैन कार्ड", "PAN card"},
	LandRecord:           {"ज़मीन का कागज़ (भूमि रिकॉर्ड)", "Land record"},
	BirthCertificate:     {"जन्म प्रमाण पत्र", "Birth certificate"},
	SchoolCertificate:    {"स्कूल/बोर्ड का प्रमाण पत्र या अंकपत्र", "School certificate or marksheet"},
	Unknown:              {"पहचान नहीं हो पाई", "Not recognised"},
}

// Label is the document type's display name.
func Label(t DocType, lang string) string {
	l, ok := typeLabels[t]
	if !ok {
		l = typeLabels[Unknown]
	}
	if lang == "en" {
		return l[1]
	}
	return l[0]
}

type msgKey int

const (
	msgNotice msgKey = iota
	msgUnknown
	msgRetake
	msgNoScheme
	msgListNeedsVerification
	msgNoDocList
	msgCoverageCaveat
	msgCoverageIntro
)

var messages = map[msgKey][2]string{
	msgNotice: {
		"ध्यान दें: यह जाँच सिर्फ़ यह देखती है कि कागज़ किस तरह का लगता है। यह कागज़ के असली या सही होने की पुष्टि नहीं करती। फ़ोटो रखी नहीं गई है।",
		"Note: this check only looks at what kind of paper this seems to be. It does not verify that the document is genuine or valid. The photo has not been kept.",
	},
	msgUnknown: {
		"यह कौन-सा कागज़ है, पहचान नहीं पाए। कृपया कागज़ का ऊपरी हिस्सा (शीर्षक) अच्छी रोशनी में, सीधा रखकर फिर से फ़ोटो लें।",
		"We could not tell what document this is. Please hold the top part (the heading) straight, in good light, and take the photo again.",
	},
	msgRetake: {
		"ठीक है। कृपया कागज़ को सीधा और साफ़ रोशनी में रखकर फिर से फ़ोटो लें।",
		"All right. Please hold the document straight, in good light, and take the photo again.",
	},
	msgNoScheme: {
		"अभी किसी योजना की बात नहीं हो रही है, इसलिए ज़रूरी कागज़ों से मिलान नहीं किया गया। पहले योजना के बारे में पूछें।",
		"No scheme is being discussed right now, so nothing was compared with a document list. Ask about a scheme first.",
	},
	msgListNeedsVerification: {
		"इस योजना के कागज़ों की सूची सत्यापित होने के बाद ही मिलान किया जा सकेगा।",
		"The documents can be compared only after this scheme's document list has been verified.",
	},
	msgNoDocList: {
		"इस योजना के रिकॉर्ड में कागज़ों की सूची नहीं है। कार्यालय में पूछें।",
		"This scheme's record has no document list. Please ask at the office.",
	},
	msgCoverageCaveat: {
		"यह मिलान सिर्फ़ कागज़ के प्रकार का है। किसका कागज़ चाहिए, कब का जारी हुआ हो जैसी शर्तें आवेदन से पहले कार्यालय में ज़रूर पूछें।",
		"This compares only the kind of document. Ask at the office before applying about conditions such as whose document is needed and how recent it must be.",
	},
	msgCoverageIntro: {
		"आपने ये कागज़ दिखाए:",
		"You have shown:",
	},
}

func msg(lang string, k msgKey) string {
	if lang == "en" {
		return messages[k][1]
	}
	return messages[k][0]
}

// Notice is the "OCR does not verify authenticity" line shown with every
// result.
func Notice(lang string) string { return msg(lang, msgNotice) }

// AskConfirm is the question after a photo: "यह <type> लगता है — क्या यह
// सही है?" (or the "could not recognise" message for Unknown).
func AskConfirm(t DocType, lang string) string {
	if t == Unknown {
		return msg(lang, msgUnknown)
	}
	if lang == "en" {
		return "This looks like a " + Label(t, lang) + " — is that right?"
	}
	return "यह " + Label(t, lang) + " लगता है — क्या यह सही है?"
}

// RetakeText is the reply when the citizen says the type is wrong.
func RetakeText(lang string) string { return msg(lang, msgRetake) }

// NoSchemeText is shown when no scheme is being discussed.
func NoSchemeText(lang string) string { return msg(lang, msgNoScheme) }

// CoverageCaveat is shown with every coverage list.
func CoverageCaveat(lang string) string { return msg(lang, msgCoverageCaveat) }

// CoverageIntro heads the list of confirmed documents.
func CoverageIntro(lang string) string { return msg(lang, msgCoverageIntro) }
