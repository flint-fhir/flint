const quickstartSnippets = {
  compose: `# Start standalone Flint FHIR R4 Server + PostgreSQL 16 + seeded 29-resource US Core demo cohort
curl -fsSL https://raw.githubusercontent.com/flint-fhir/flint/main/docker-compose.yml -o docker-compose.yml
docker compose --profile seed up -d

# Explore the live FHIR R4 API & 78-Check Conformance Suite:
open http://localhost:8080/fhir/r4/demo/$docs
curl -s http://localhost:8080/fhir/r4/demo/$certify | jq .result`,

  container: `# Run the multi-arch container (linux/amd64, linux/arm64) against any PostgreSQL 15+ database
docker run --rm -p 8080:8080 \\
  -e FLINT_POSTGRES_DSN="postgres://flint:flint@host.docker.internal:5432/flint?sslmode=disable" \\
  ghcr.io/flint-fhir/flint:latest

# Seed the US Core cohort & execute the 78-check conformance certification CLI
docker run --rm --entrypoint /usr/local/bin/seed ghcr.io/flint-fhir/flint:latest -url http://host.docker.internal:8080 -tenant demo
docker run --rm --entrypoint /usr/local/bin/certify ghcr.io/flint-fhir/flint:latest -url http://host.docker.internal:8080 -tenant demo`,

  binary: `# Install single binary directly with Go 1.25+ (embeds all Atlas SQL migrations)
go install github.com/flint-fhir/flint/cmd/flintd@latest

FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \\
FLINT_ADDR=":8080" \\
  flintd`
};

const igCatalog = [
  {
    title: "HL7 US Core v6.1.0 & v7.0.0 (USCDI v3 & v4)",
    domain: "Core & ONC",
    desc: "All 26 US Core profiles, Clinical Notes ($docref), Observation/$lastn & $stats, X-Provenance header & /$provenance lineage, and granular SMART v2 sub-resource scopes.",
    ops: ["DocumentReference/$docref", "Observation/$lastn", "Observation/$stats", "/{type}/{id}/$provenance"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/DocumentReference/$docref?patient=example-patient-1&on-demand=true' | jq ."
  },
  {
    title: "ONC (g)(10) Bulk Data Access v2.0.0 & NDJSON Import",
    domain: "Core & ONC",
    desc: "System (/$export), Patient (/Patient/$export), Group (/Group/{id}/$export), Single-Patient ($export), Postgres-persisted async job polling, and Bulk NDJSON $import.",
    ops: ["GET /$export", "GET /Patient/$export", "GET /Group/{id}/$export", "POST /$import"],
    curl: "curl -i -H 'Prefer: respond-async' 'http://localhost:8080/fhir/r4/demo/$export?_type=Patient,Condition,Observation'"
  },
  {
    title: "SMART on FHIR v2.0.0, Backend Services (RFC 7523) & HL7 FAST / UDAP Security",
    domain: "Core & ONC",
    desc: "Built-in OAuth2 PKCE (S256), asymmetric ES384/RS384/RS256 private_key_jwt client assertions, HL7 FAST UDAP v1.0.0 Trusted Dynamic Client Registration ($udap-register, $udap-trust-summary, /.well-known/udap), and strict Patient compartment isolation.",
    ops: ["GET /.well-known/smart-configuration", "GET /.well-known/udap", "Organization/$udap-register", "Organization/{id}/$udap-trust-summary", "POST /oauth2/token"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/.well-known/udap' | jq ."
  },
  {
    title: "HL7 Da Vinci Burden Reduction (CRD, DTR & PAS v2.0.1)",
    domain: "Da Vinci Payer",
    desc: "Coverage Requirements Discovery ($crd), Documentation Templates & Rules ($dtr-questionnaire-package), Prior Authorization ($submit, $pas-inquire), and $burden-reduction-summary.",
    ops: ["ServiceRequest/$crd", "Questionnaire/$dtr-questionnaire-package", "Claim/$submit", "Claim/$pas-inquire", "Patient/$burden-reduction-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$burden-reduction-summary' | jq ."
  },
  {
    title: "HL7 Da Vinci Clinical Data Exchange (CDex v2.0.0)",
    domain: "Da Vinci Payer",
    desc: "Clinical attachments ($submit-attachment, Task/$cdex-submit-attachment), Task-based requests ($cdex-request, $cdex-poll), and JWS digital signatures ($cdex-sign, $cdex-verify-signature).",
    ops: ["Task/$cdex-request", "Task/{id}/$cdex-poll", "Task/$cdex-submit-attachment", "Bundle/$cdex-sign", "Bundle/$cdex-verify-signature"],
    curl: "curl -s -X POST 'http://localhost:8080/fhir/r4/demo/Task/$cdex-submit-attachment' -H 'Content-Type: application/fhir+json' -d '{\"patient\":\"Patient/example-patient-1\"}' | jq ."
  },
  {
    title: "CMS-0057-F Interoperability Final Rule: Da Vinci PDex v2.1.0 & Member Attribution (ATR)",
    domain: "Da Vinci Payer",
    desc: "Provider Access API ($pdex-provider-member-match, $pdex-provider-everything), Payer-to-Payer Data Exchange ($pdex-payer-exchange, $pdex-payer-summary), member matching ($member-match, $bulk-member-match), and attribution rosters ($confirm-attribution-list, $atr-summary).",
    ops: ["Group/$pdex-provider-member-match", "Patient/{id}/$pdex-provider-everything", "Patient/$pdex-payer-exchange", "Patient/{id}/$pdex-payer-summary", "Group/{id}/$confirm-attribution-list", "Group/{id}/$atr-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$pdex-provider-everything' | jq ."
  },
  {
    title: "HL7 Da Vinci Risk Adjustment (RA v2.0.0) & VBPR (v1.0.0)",
    domain: "Da Vinci Payer",
    desc: "CMS-HCC coding gap evaluation ($evaluate-risk-condition-category, $ra-evaluate-risk, $ra-coding-gap-report, $ra-remediate) and Value-Based Performance Reporting ($vbpr-performance, $vbpr-attest).",
    ops: ["MeasureReport/$ra-evaluate-risk", "Patient/{id}/$ra-coding-gap-report", "MeasureReport/$vbpr-performance", "MeasureReport/{id}/$vbpr-attest"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$ra-coding-gap-report' | jq ."
  },
  {
    title: "HL7 Da Vinci Patient Cost Transparency (PCT v2.0.0) & Drug Formulary",
    domain: "Da Vinci Payer",
    desc: "Good Faith Estimates (Claim/$gfe-submit, Patient/$gfe), multi-provider coordination (Task/$pct-gfe-coordination), AEOB packets ($pct-aeob-summary), and US Drug Formulary ($formulary-lookup, $formulary-alternatives).",
    ops: ["Claim/$gfe-submit", "ExplanationOfBenefit/$pct-aeob-summary", "Task/$pct-gfe-coordination", "MedicationKnowledge/$formulary-lookup", "Coverage/$formulary-coverage"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/ExplanationOfBenefit/$pct-aeob-summary?patient=Patient/example-patient-1' | jq ."
  },
  {
    title: "CARIN Blue Button (v2.0.0) & Real-Time Pharmacy Benefit Check (RTPBC)",
    domain: "Da Vinci Payer",
    desc: "Consumer-Directed Payer Data Exchange EOB summaries ($carin-bb, $carin-bb-summary, $carin-bb-claims) and Real-Time Pharmacy Benefit Check (Claim/$rtpbc-submit, $rtpbc-coverage, $rtpbc-summary).",
    ops: ["Patient/{id}/$carin-bb", "ExplanationOfBenefit/$carin-bb-claims", "Claim/$rtpbc-submit", "ClaimResponse/$rtpbc-coverage", "Patient/{id}/$rtpbc-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/ExplanationOfBenefit/$carin-bb-claims?patient=Patient/example-patient-1' | jq ."
  },
  {
    title: "HL7 CodeX / mCODE Oncology, Genomics, Trial Matching & RTTD",
    domain: "Oncology & Genomics",
    desc: "mCODE v3.0.0 staging ($stage-cancer, $mcode-disease-status, $mcode-summary), Radiotherapy & RTTD ($mcode-radiotherapy-record, $codex-rttd-plan, $codex-rttd-summary), Genomics ($mcode-genomics-report), and Clinical Trial Matching ($mcode-trial-match, $mcode-trial-summary).",
    ops: ["Patient/{id}/$mcode-summary", "Condition/{id}/$stage-cancer", "ServiceRequest/$codex-rttd-plan", "Patient/{id}/$codex-rttd-summary", "Patient/{id}/$mcode-trial-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$codex-rttd-summary' | jq ."
  },
  {
    title: "HL7 Clinical Genomics Reporting (v3.0.0)",
    domain: "Oncology & Genomics",
    desc: "Subject molecular sequence variant discovery ($find-subject-variants, LOINC 69548-6) and pharmacogenomic therapeutic implications ($find-subject-tx-implications, LOINC 51963-7).",
    ops: ["Patient/{id}/$find-subject-variants", "Patient/{id}/$find-subject-tx-implications", "DiagnosticReport/$mcode-genomics-report", "Patient/{id}/$mcode-genomics-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$find-subject-variants?genes=BRCA1' | jq ."
  },
  {
    title: "HL7 Public Health Situational Awareness (SANER v1.0.1) & CDC NHSN",
    domain: "Public Health",
    desc: "Hospital bed, ICU, and ventilator situational awareness (MeasureReport/$saner-report-capacity, Location/$saner-capacity-summary) and CDC NHSN HAI/AUR/ACH Digital Quality Measures ($nhsn-dqm-evaluate, $nhsn-ach-summary, $nhsn-aur-submit).",
    ops: ["MeasureReport/$saner-report-capacity", "Location/{id}/$saner-capacity-summary", "Measure/$nhsn-dqm-evaluate", "Patient/{id}/$nhsn-ach-summary", "MeasureReport/$nhsn-aur-submit"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Location/saner-loc-1/$saner-capacity-summary' | jq ."
  },
  {
    title: "HL7 Public Health eCR (v2.1.2), ELR, IIS, NBS, VRDR, BFDR & CCRR",
    domain: "Public Health",
    desc: "Electronic Initial Case Reporting ($ecr-submit, $eicr-document, $ersd-lookup), Reportable Labs ($elr-submit, $elr-summary), Immunization IIS ($iis-submit, $iis-history), Newborn Screening ($nbs-submit), Vital Records Birth/Death ($vrdr-submit, $bfdr-submit), and Central Cancer Registry ($cancer-registry-submit).",
    ops: ["Bundle/$ecr-submit", "Patient/{id}/$ecr-summary", "Patient/{id}/$elr-summary", "Patient/{id}/$iis-history", "Patient/{id}/$nbs-summary", "Bundle/$cancer-registry-submit"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$ecr-summary' | jq ."
  },
  {
    title: "HL7 PACIO Post-Acute Care (Functional, Cognitive, ADI, PFE, TOC & SPLASCH)",
    domain: "Post-Acute & SDOH",
    desc: "Functional & Cognitive Status ($pacio-assess, $pacio-cognitive-summary), Advance Directives ($adi-record, $adi-document, $adi-verify), Personal Functioning ($pacio-pfe-summary), Transitions of Care ($pacio-toc-bundle), Re-Assessment Timepoints ($pacio-timepoints), and SPLASCH ($pacio-splasch-record, $pacio-splasch-summary).",
    ops: ["Patient/{id}/$pacio-summary", "Composition/$adi-document", "Composition/$pacio-toc-bundle", "Patient/{id}/$pacio-pfe-summary", "Patient/{id}/$pacio-splasch-summary"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$pacio-splasch-summary' | jq ."
  },
  {
    title: "HL7 Gravity Project SDOH Clinical Care (v2.2.0) & Physical Activity",
    domain: "Post-Acute & SDOH",
    desc: "Closed-loop Social Determinants of Health screening ($sdoh-assess), referrals (ServiceRequest/$sdoh-referral), CBO fulfillment (Task/$sdoh-fulfill), privacy consent (Consent/$sdoh-consent-record), care bundles ($sdoh-care-bundle), and Physical Activity ($physical-activity-summary).",
    ops: ["QuestionnaireResponse/$sdoh-assess", "ServiceRequest/$sdoh-referral", "Task/$sdoh-fulfill", "Consent/$sdoh-consent-record", "Patient/{id}/$sdoh-care-bundle"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$sdoh-care-bundle' | jq ."
  },
  {
    title: "HL7 Vulcan Clinical Research (RWD, SOA, eSource, ePI & FDA ICSR)",
    domain: "Research & Directory",
    desc: "Real-World Data pharmacoepidemiology cohorts ($vulcan-rwd-cohort, $vulcan-rwd-summary), Schedule of Activities ($vulcan-soa-timeline), eSource CRF extraction ($vulcan-esource-extract), ePI ($epi-document), and FDA E2B(R3) ICSR ($vulcan-icsr-report).",
    ops: ["ResearchStudy/$vulcan-rwd-cohort", "Patient/{id}/$vulcan-rwd-summary", "ResearchSubject/{id}/$vulcan-soa-timeline", "ResearchSubject/$vulcan-esource-extract", "AdverseEvent/$vulcan-icsr-report"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Patient/example-patient-1/$vulcan-rwd-summary' | jq ."
  },
  {
    title: "ONC FAST National Directory (NDH v1.0.0), Plan-Net & IHE (PIXm/PDQm/MHD)",
    domain: "Research & Directory",
    desc: "National Healthcare Directory verification & exchange (VerificationResult/$ndh-verify, Organization/$ndh-directory-summary, Endpoint/$fast-endpoint-query, OrganizationAffiliation/$fast-network-query), Digital Identity ($idi-match), and IHE PIXm/PDQm/MHD ($ihe-pix, $ihe-pdqm, $iti-65, $iti-67, $ihe-iti-68).",
    ops: ["VerificationResult/$ndh-verify", "Organization/{id}/$ndh-directory-summary", "Endpoint/$fast-endpoint-query", "Patient/$ihe-pdqm", "DocumentReference/$iti-67"],
    curl: "curl -s 'http://localhost:8080/fhir/r4/demo/Organization/example-org-1/$ndh-directory-summary' | jq ."
  }
];

let activeDomain = "All";
let searchQuery = "";

function selectQuickstartTab(tabKey) {
  document.querySelectorAll(".tab-btn").forEach((btn) => {
    btn.setAttribute("aria-selected", btn.dataset.tab === tabKey ? "true" : "false");
  });
  const codeEl = document.getElementById("quickstartCode");
  if (codeEl && quickstartSnippets[tabKey]) {
    codeEl.textContent = quickstartSnippets[tabKey];
  }
}

function copyQuickstart() {
  const codeEl = document.getElementById("quickstartCode");
  const btn = document.getElementById("copyQuickstartBtn");
  if (!codeEl || !navigator.clipboard) return;
  navigator.clipboard.writeText(codeEl.textContent).then(() => {
    const orig = btn.textContent;
    btn.textContent = "Copied!";
    setTimeout(() => {
      btn.textContent = orig;
    }, 1500);
  });
}

function renderIGCatalog() {
  const grid = document.getElementById("igGrid");
  const countEl = document.getElementById("igCount");
  if (!grid) return;

  const q = searchQuery.trim().toLowerCase();
  const filtered = igCatalog.filter((item) => {
    const matchesDomain = activeDomain === "All" || item.domain === activeDomain;
    if (!matchesDomain) return false;
    if (!q) return true;
    const hay = (item.title + " " + item.domain + " " + item.desc + " " + item.ops.join(" ")).toLowerCase();
    return hay.includes(q);
  });

  if (countEl) {
    countEl.textContent = `Showing ${filtered.length} of ${igCatalog.length} Implementation Guide Suites`;
  }

  grid.innerHTML = filtered
    .map(
      (item) => `
      <article class="ig-card">
        <div>
          <div class="ig-card-header">
            <h3 class="ig-title">${item.title}</h3>
            <span class="ig-domain">${item.domain}</span>
          </div>
          <p class="ig-desc">${item.desc}</p>
        </div>
        <div class="ig-ops">
          ${item.ops.map((op) => `<span class="op-chip">${op}</span>`).join("")}
        </div>
        <pre class="ig-curl"><code>${item.curl}</code></pre>
      </article>
    `
    )
    .join("");
}

function setDomainFilter(domain) {
  activeDomain = domain;
  document.querySelectorAll(".pill-btn").forEach((btn) => {
    btn.setAttribute("aria-pressed", btn.dataset.domain === domain ? "true" : "false");
  });
  renderIGCatalog();
}

document.addEventListener("DOMContentLoaded", () => {
  selectQuickstartTab("compose");
  renderIGCatalog();

  const searchInput = document.getElementById("igSearchInput");
  if (searchInput) {
    searchInput.addEventListener("input", (e) => {
      searchQuery = e.target.value || "";
      renderIGCatalog();
    });
  }
});
