export type SupplierCredentialBindingState = 'bound' | 'unconfigured' | 'ambiguous';

export type SupplierCredentialBinding<Supplier, Credential> = {
	supplier: Supplier;
	credential?: Credential;
	state: SupplierCredentialBindingState;
};

const canonicalCredentialID = /^[1-9][0-9]*$/;

export function projectSupplierCredentialBindings<
	Supplier extends { credentialId?: string | null },
	Credential extends { id: string },
>(suppliers: readonly Supplier[], credentials: readonly Credential[]) {
	const referencedCredentialIDs = new Set<string>();
	const bindings: SupplierCredentialBinding<Supplier, Credential>[] = suppliers.map((supplier) => {
		const credentialID = supplier.credentialId;
		if (typeof credentialID !== 'string' || !canonicalCredentialID.test(credentialID)) {
			return { supplier, state: 'unconfigured' };
		}
		referencedCredentialIDs.add(credentialID);
		const matches = credentials.filter((credential) => credential.id === credentialID);
		if (matches.length === 1) {
			return { supplier, credential: matches[0], state: 'bound' };
		}
		return { supplier, state: matches.length === 0 ? 'unconfigured' : 'ambiguous' };
	});

	return {
		bindings,
		orphans: credentials.filter((credential) => !referencedCredentialIDs.has(credential.id)),
	};
}
