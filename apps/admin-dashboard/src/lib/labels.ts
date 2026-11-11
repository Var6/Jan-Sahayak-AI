// Bilingual labels for citizen-facing terms shown to staff. These are UI
// translations of category names, not facts about any scheme.

export interface Bilingual {
  en: string;
  hi: string;
}

export const CATEGORY_LABELS: Record<string, Bilingual> = {
  pension: { en: "Pension", hi: "पेंशन" },
  scholarship: { en: "Scholarship", hi: "छात्रवृत्ति" },
  education: { en: "Education", hi: "शिक्षा" },
  farmer: { en: "Farmer", hi: "किसान" },
  loan: { en: "Loan", hi: "ऋण" },
  subsidy: { en: "Subsidy", hi: "अनुदान" },
  self_employment: { en: "Self-employment", hi: "स्वरोज़गार" },
  housing: { en: "Housing", hi: "आवास" },
  health: { en: "Health", hi: "स्वास्थ्य" },
  food_security: { en: "Food security", hi: "खाद्य सुरक्षा" },
  certificate: { en: "Certificate", hi: "प्रमाण पत्र" },
  identity_document: { en: "Identity document", hi: "पहचान पत्र" },
  women_child: { en: "Women & child", hi: "महिला एवं बाल" },
  disability: { en: "Disability", hi: "दिव्यांगता" },
  labour: { en: "Labour", hi: "श्रम" },
};

export const LANGUAGE_LABELS: Record<string, Bilingual> = {
  hi: { en: "Hindi", hi: "हिन्दी" },
  en: { en: "English", hi: "अंग्रेज़ी" },
};

export const RESOLUTION_LABELS: Record<string, string> = {
  answered: "Answered (verified facts)",
  unverified_info: "Unverified info only",
  escalated: "Escalated to staff/helpline",
  unresolved: "Unresolved",
  redirected: "Redirected (off-topic)",
  crisis: "Crisis response",
};

export const END_REASON_LABELS: Record<string, string> = {
  citizen_ended: "Citizen ended",
  inactivity_timeout: "Inactivity timeout",
  device_shutdown: "Device shutdown",
  error: "Error",
};

export const SOURCE_LABELS: Record<string, string> = {
  voice: "Voice",
  text: "Typed text",
  option: "Tapped option",
};

export function bilingual(map: Record<string, Bilingual>, key: string): Bilingual {
  return map[key] ?? { en: key || "(none)", hi: "" };
}
