import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import App from './App'

describe('App', () => {
  beforeEach(() => sessionStorage.clear())

  it('starts with the API key session screen', () => {
    render(<App />)
    expect(screen.getByRole('heading', { name: /欢迎回到你的/ })).toBeInTheDocument()
    expect(screen.getByLabelText('API Key')).toHaveAttribute('type', 'password')
    expect(screen.getByRole('button', { name: '进入 NexusMail' })).toBeDisabled()
  })
})
