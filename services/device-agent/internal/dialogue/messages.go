package dialogue

// Fixed kiosk wording (Hindi primary, English). Nothing here is a
// government fact: no scheme names, amounts, documents, URLs or phone
// numbers, except the national emergency number 112 (see crisis.go and
// docs/AI_GUARDRAILS.md). Scheme content comes only from knowledge records.

type bi struct{ hi, en string }

func (b bi) in(l Lang) string {
	if l == English {
		return b.en
	}
	return b.hi
}

var m = struct {
	greeting, greetingSpeech, unknownScheme, unknownSchemeAnswer, menuPrompt, moreHelp, notUnderstood, repeatAudio, tooManyMisses,
	keyboard, humanHelp, goodbye, thanks, help, offTopic, injection,
	sensitive, noVerified, pickScheme, invalidOption, langSwitched,
	ineligibleNote, aadhaarNotCaste, cardsBelow, cardsBelowSpeech,
	clarifyBetween, factualDisclaimer, factualDisclaimerNoDate,
	technical, suggestionsFromSearch, noBenefitInfo, noDocInfo bi
	sectionEligibility, sectionBenefits, sectionDocs, sectionOptionalDocs,
	sectionAltEvidence, sectionSteps, sectionWhere, sectionFees, sectionNote,
	sectionInfo bi
	optRepeat, optEnd, optYes, optNo, optMoreYes, optMoreNo, optKeyboard,
	optHuman, optMenu, optOther bi
}{
	greeting: bi{
		"नमस्ते! मैं जन सहायक हूँ — एक कंप्यूटर सहायक, कोई सरकारी अधिकारी नहीं। मैं सरकारी योजनाओं, पेंशन, लोन और प्रमाण पत्रों की जानकारी देने में मदद करता हूँ। मैं कोई आवेदन जमा नहीं करता। कृपया यहाँ आधार नंबर, OTP, PIN या बैंक खाता नंबर न बताएँ। बताइए, आपको किस बारे में जानना है? आप बोल सकते हैं या नीचे बटन दबा सकते हैं।",
		"Hello! I am Jan Sahayak — a computer assistant, not a government official. I help with information about government schemes, pensions, loans and certificates. I do not submit applications. Please do not tell me your Aadhaar number, OTP, PIN or bank account number. What would you like to know? You can speak or tap a button below.",
	},
	// greetingSpeech is what TTS says at session start: at most ~10 s of
	// speech (≈25 words). It keeps the two things that must be HEARD — "a
	// computer assistant, not an official" and "do not tell me secrets";
	// the rest of the greeting is on screen only.
	greetingSpeech: bi{
		"नमस्ते! मैं जन सहायक हूँ, एक कंप्यूटर सहायक, सरकारी अधिकारी नहीं। आधार नंबर, OTP या PIN न बताएँ। बताइए, किस बारे में जानना है?",
		"Hello! I am Jan Sahayak, a computer assistant, not a government official. Never share your Aadhaar, OTP or PIN. What do you need?",
	},
	unknownScheme: bi{"इस योजना की सत्यापित जानकारी मेरे पास नहीं है। इससे मिलते-जुलते विकल्प नीचे हैं।",
		"I do not have verified information on that scheme. Related options are below."},
	unknownSchemeAnswer: bi{"ध्यान दें: आपने जिस योजना का नाम लिया था, उसकी सत्यापित जानकारी मेरे पास नहीं है। नीचे दी गई जानकारी एक दूसरी योजना की है।",
		"Please note: I do not have verified information on the scheme you named. The information below is about a different scheme."},
	menuPrompt:    bi{"बताइए, आपको किस बारे में जानना है?", "What would you like to know about?"},
	moreHelp:      bi{"क्या आपको और मदद चाहिए?", "Do you need any more help?"},
	notUnderstood: bi{"माफ़ कीजिए, मैं समझ नहीं पाया। कृपया नीचे दिए विषयों में से चुनें या दोबारा बताइए।", "Sorry, I did not understand. Please choose one of the topics below or say it again."},
	repeatAudio:   bi{"माफ़ कीजिए, आवाज़ साफ़ नहीं आई। कृपया दोबारा बोलिए।", "Sorry, I could not hear that clearly. Please say it again."},
	tooManyMisses: bi{"माफ़ कीजिए, मुझे समझने में दिक्कत हो रही है। आप स्क्रीन पर टाइप कर सकते हैं, या किसी व्यक्ति से मदद ले सकते हैं।", "Sorry, I am having trouble understanding. You can type on the screen, or ask a person for help."},
	keyboard:      bi{"ठीक है। कृपया स्क्रीन पर अपना सवाल टाइप कीजिए।", "OK. Please type your question on the screen."},
	humanHelp:     bi{"मैं एक कंप्यूटर सहायक हूँ। किसी व्यक्ति से मदद के लिए कृपया यहाँ मौजूद सहायक (यदि उपलब्ध हों) से बात करें, या अपने नज़दीकी RTPS काउंटर, CSC (जन सेवा केंद्र) या प्रखंड कार्यालय जाएँ।", "I am a computer assistant. For help from a person, please speak to the helper here (if available), or visit your nearest RTPS counter, CSC (Common Service Centre) or block office."},
	goodbye:       bi{"धन्यवाद! जन सहायक का उपयोग करने के लिए शुक्रिया। आपकी बातचीत यहाँ सहेजी नहीं जाती। नमस्ते!", "Thank you for using Jan Sahayak. Your conversation is not saved here. Goodbye!"},
	thanks:        bi{"आपका स्वागत है।", "You are welcome."},
	help:          bi{"मैं इन विषयों पर जानकारी दे सकता हूँ: लोन, पेंशन, छात्रवृत्ति, किसान योजनाएँ, प्रमाण पत्र (जाति, आय, निवास, जन्म, मृत्यु), राशन कार्ड, आवास, स्वास्थ्य बीमा, स्वरोजगार, महिला और बच्चों की योजनाएँ, दिव्यांग जन की योजनाएँ और पहचान पत्र। बोलिए या बटन दबाइए।", "I can give information on: loans, pensions, scholarships, farmer schemes, certificates (caste, income, residence, birth, death), ration cards, housing, health insurance, self-employment, schemes for women and children, disability schemes and identity documents. Speak or tap a button."},
	offTopic:      bi{"माफ़ कीजिए, मैं केवल सरकारी योजनाओं, प्रमाण पत्रों और सरकारी सेवाओं की जानकारी दे सकता हूँ।", "Sorry, I can only help with government schemes, certificates and public services."},
	injection:     bi{"मैं केवल सरकारी योजनाओं और सेवाओं की जानकारी दे सकता हूँ, और अपने नियम नहीं बदल सकता। कृपया बताइए आपको किस योजना या प्रमाण पत्र के बारे में जानना है।", "I can only give information about government schemes and services, and I cannot change my rules. Please tell me which scheme or certificate you want to know about."},
	sensitive:     bi{"ध्यान दें: कृपया यहाँ आधार नंबर, OTP, PIN, पासवर्ड, बैंक खाता या कार्ड नंबर जैसी निजी जानकारी न बताएँ। जानकारी देने के लिए मुझे इनकी ज़रूरत नहीं है, और मैंने इसे सहेजा नहीं है।", "Please note: do not share your Aadhaar number, OTP, PIN, password, bank account or card number here. I do not need them to give information, and I have not saved it."},
	noVerified:    bi{"इस विषय पर मेरे पास अभी कोई सत्यापित जानकारी नहीं है। कृपया अपने नज़दीकी RTPS काउंटर, CSC (जन सेवा केंद्र) या प्रखंड कार्यालय पर पूछें।", "I do not have verified information on this yet. Please ask at your nearest RTPS counter, CSC (Common Service Centre) or block office."},
	pickScheme:    bi{"मुझे ये योजनाएँ/सेवाएँ मिलीं। आप किसके बारे में जानना चाहेंगे?", "I found these schemes/services. Which one would you like to know about?"},
	invalidOption: bi{"कृपया स्क्रीन पर दिए विकल्पों में से चुनें।", "Please choose one of the options on the screen."},
	langSwitched:  bi{"ठीक है, अब मैं हिंदी में बात करूँगा।", "OK, I will continue in English."},
	ineligibleNote: bi{"आपके जवाबों के हिसाब से शायद आप इसकी सभी शर्तें पूरी न करें। अंतिम फ़ैसला संबंधित कार्यालय करेगा — कृपया वहाँ पुष्टि करें।",
		"Based on your answers you may not meet all the conditions. The office makes the final decision — please confirm there."},
	aadhaarNotCaste:  bi{"आधार कार्ड पहचान का प्रमाण है, जाति का प्रमाण नहीं।", "Aadhaar proves identity, not caste."},
	cardsBelow:       bi{"पूरी जानकारी नीचे कार्ड में दी गई है।", "Full details are shown in the card below."},
	cardsBelowSpeech: bi{"ज़रूरी दस्तावेज़ और आवेदन की जानकारी स्क्रीन पर दिखाई गई है।", "The documents and how to apply are shown on the screen."},
	clarifyBetween:   bi{"आप किस बारे में जानना चाहते हैं?", "Which of these do you mean?"},
	factualDisclaimer: bi{"यह जानकारी %s को सत्यापित की गई थी। यह कियोस्क आवेदन जमा नहीं करता और मंज़ूरी की कोई गारंटी नहीं देता — अंतिम निर्णय संबंधित कार्यालय का होता है।",
		"This information was verified on %s. This kiosk does not submit applications and cannot guarantee approval — the office makes the final decision."},
	factualDisclaimerNoDate: bi{"यह कियोस्क आवेदन जमा नहीं करता और मंज़ूरी की कोई गारंटी नहीं देता — अंतिम निर्णय संबंधित कार्यालय का होता है।",
		"This kiosk does not submit applications and cannot guarantee approval — the office makes the final decision."},
	technical:             bi{"माफ़ कीजिए, अभी तकनीकी दिक्कत है। कृपया अपने नज़दीकी RTPS काउंटर, CSC या प्रखंड कार्यालय पर पूछें।", "Sorry, there is a technical problem. Please ask at your nearest RTPS counter, CSC or block office."},
	suggestionsFromSearch: bi{"क्या आप इनमें से किसी के बारे में पूछ रहे हैं?", "Are you asking about one of these?"},
	noBenefitInfo:         bi{"इस बारे में सत्यापित जानकारी उपलब्ध नहीं है। कृपया कार्यालय में पुष्टि करें।", "Verified information on this is not available. Please confirm at the office."},
	noDocInfo:             bi{"दस्तावेज़ों की सत्यापित सूची उपलब्ध नहीं है। कृपया कार्यालय में पुष्टि करें।", "A verified document list is not available. Please confirm at the office."},

	sectionEligibility:  bi{"कौन ले सकता है", "Who can apply"},
	sectionBenefits:     bi{"क्या मिलता है", "Benefits"},
	sectionDocs:         bi{"ज़रूरी दस्तावेज़", "Required documents"},
	sectionOptionalDocs: bi{"अन्य उपयोगी दस्तावेज़", "Other useful documents"},
	sectionAltEvidence:  bi{"दस्तावेज़ न हो तो", "If you lack a document"},
	sectionSteps:        bi{"आवेदन कैसे करें", "How to apply"},
	sectionWhere:        bi{"कहाँ आवेदन करें", "Where to apply"},
	sectionFees:         bi{"शुल्क", "Fees"},
	sectionNote:         bi{"ध्यान दें", "Please note"},
	sectionInfo:         bi{"जानकारी", "Information"},

	optRepeat:   bi{"फिर से सुनें", "Repeat"},
	optEnd:      bi{"बातचीत खत्म करें", "End"},
	optYes:      bi{"हाँ", "Yes"},
	optNo:       bi{"नहीं", "No"},
	optMoreYes:  bi{"हाँ, और जानना है", "Yes, more help"},
	optMoreNo:   bi{"नहीं, धन्यवाद", "No, thank you"},
	optKeyboard: bi{"टाइप करके लिखें", "Type instead"},
	optHuman:    bi{"किसी व्यक्ति से मदद", "Help from a person"},
	optMenu:     bi{"मुख्य विषय", "Main topics"},
	optOther:    bi{"अन्य विषय", "Other topics"},
}

// Topic labels for menu options.
var topicLabel = map[string]bi{
	IntentLoan:           {"लोन / ऋण", "Loan"},
	IntentPension:        {"पेंशन", "Pension"},
	IntentCertificate:    {"प्रमाण पत्र", "Certificate"},
	IntentFarmer:         {"किसान योजना", "Farmer schemes"},
	IntentScholarship:    {"छात्रवृत्ति / पढ़ाई", "Scholarship / education"},
	IntentRationCard:     {"राशन कार्ड", "Ration card"},
	IntentHousing:        {"आवास / घर", "Housing"},
	IntentHealth:         {"स्वास्थ्य बीमा / इलाज", "Health insurance"},
	IntentSelfEmployment: {"स्वरोजगार / व्यवसाय", "Self-employment / business"},
	IntentWomenChild:     {"महिला और बच्चे", "Women and children"},
	IntentDisability:     {"दिव्यांग जन", "Persons with disability"},
	IntentIdentity:       {"पहचान पत्र", "Identity documents"},
}

// Redirect lines for disallowed topics (appended to m.offTopic).
var offTopicLine = map[string]bi{
	IntentMedical:    {"बीमारी या दवा के बारे में कृपया डॉक्टर या नज़दीकी स्वास्थ्य केंद्र से सलाह लें। आपात स्थिति में 112 पर कॉल करें।", "For illness or medicines please consult a doctor or the nearest health centre. In an emergency call 112."},
	IntentInvestment: {"मैं पैसे लगाने या निवेश की सलाह नहीं देता।", "I do not give investment advice."},
	IntentPolitics:   {"मैं राजनीति या चुनाव के बारे में बात नहीं करता।", "I do not discuss politics or elections."},
	IntentReligion:   {"मैं धर्म के विषयों पर बात नहीं करता।", "I do not discuss religion."},
	IntentIllegal:    {"मैं रिश्वत, हथियार या ऐसे किसी काम में मदद नहीं कर सकता।", "I cannot help with bribes, weapons or anything like that."},
}
