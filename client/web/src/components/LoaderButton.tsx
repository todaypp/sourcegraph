import classnames from 'classnames'
import React from 'react'

import { LoadingSpinner } from '@sourcegraph/react-loading-spinner'

interface Props extends React.DetailedHTMLProps<React.ButtonHTMLAttributes<HTMLButtonElement>, HTMLButtonElement> {
    loading: boolean
    label: string
    alwaysShowLabel: boolean
}

export const LoaderButton: React.FunctionComponent<Partial<Props>> = ({
    loading,
    label,
    alwaysShowLabel,
    ...props
}) => (
    <button
        {...props}
        className={classnames(props.className, 'd-flex justify-content-center align-items-center')}
        // eslint-disable-next-line react/button-has-type
        type={props.type ?? 'button'}
    >
        {loading ? (
            <>
                <LoadingSpinner className="icon-inline" />
                {alwaysShowLabel && <span className="ml-1">{label}</span>}
            </>
        ) : (
            label
        )}
    </button>
)
